// Needletail's Luau shim (docs/needletail.md §5.2): the C++ side of luau.wasm. It compiles Luau
// source (never bytecode) and runs it in a sandboxed state, with the host reached only through
// env.host_call / env.host_result, JSON both ways. Built by build.sh with wasi-sdk; Luau's errors
// are C++ exceptions on the WebAssembly exception-handling proposal, so every error is raised and
// caught inside the module and nothing unwinds through the host.
//
// Exports (all JSON is UTF-8):
//   nt_alloc(n) -> ptr, nt_free(ptr)          memory the host writes source and specs into
//   nt_out_ptr() / nt_out_len()               the last call's output
//   nt_check(src, len) -> 0 | 1               compile only; output: the error message
//   nt_run(src, len, spec, spec_len, mem_cap, timeout_ms, budget) -> 0 | 1
//       spec: {"name": chunk name, "entry": "" | function name, "args": any,
//              "bindings": [{"name": "fs.read", "global": "read_file"|"", "params": ["path"],
//                            "spread": bool, "local": bool}]}
//       output: {"ok":true,"v":<result>} or {"ok":false,"err":{"code":…,"msg":…}}
//
// Imports:
//   env.host_call(name_ptr, name_len, in_ptr, in_len) -> handle (< 0: refused)
//   env.host_result(handle, out_ptr, cap) -> size (when size > cap nothing is copied)

#include "lua.h"
#include "lualib.h"
#include "luacode.h"
#include "Luau/Common.h"

#include <math.h>
#include <stdint.h>
#include <stdio.h>
#include <stdlib.h>
#include <string.h>
#include <time.h>

#include <new>
#include <string>

// The parser's recursion limit, lowered from Luau's 1000: each level is several wasm frames, and
// the interpreter that runs this module allows 2000 (wazero's callStackCeiling).
LUAU_FASTINT(LuauRecursionLimit)

extern "C" {
__attribute__((import_module("env"), import_name("host_call"))) int32_t host_call(const char* name, int32_t name_len,
                                                                                  const char* in, int32_t in_len);
__attribute__((import_module("env"), import_name("host_result"))) int32_t host_result(int32_t handle, char* out,
                                                                                      int32_t cap);

// WASI has no process clocks: clock() (os.clock, math.random's seed) is the monotonic clock.
clock_t clock(void)
{
    struct timespec ts;
    clock_gettime(CLOCK_MONOTONIC, &ts);
    return (clock_t)((long long)ts.tv_sec * CLOCKS_PER_SEC + (long long)ts.tv_nsec / (1000000000LL / CLOCKS_PER_SEC));
}
}

namespace
{

const size_t kMaxSource = 1 << 20;      // bytes of source
const size_t kMaxJSON = 16 << 20;       // bytes one encode may produce
const int kMaxDepth = 64;               // JSON nesting
const int kRecursionLimit = 200;        // parser and compiler nesting
const char* kNullKey = "needletail.null"; // registry key of nt.json.null

std::string g_out;

// ---- the run's limits ------------------------------------------------------------------------

size_t g_used = 0, g_cap = 48u << 20;
int64_t g_budget = 0, g_count = 0, g_deadline = 0;
enum Trip
{
    kNone,
    kTimeout,
    kBudget
};
Trip g_trip = kNone;

int64_t now_ns()
{
    struct timespec ts;
    clock_gettime(CLOCK_MONOTONIC, &ts);
    return (int64_t)ts.tv_sec * 1000000000LL + ts.tv_nsec;
}

void* counting_alloc(void*, void* ptr, size_t osize, size_t nsize)
{
    if (nsize == 0)
    {
        free(ptr);
        g_used -= osize;
        return NULL;
    }
    if (nsize > osize && g_used + (nsize - osize) > g_cap)
        return NULL; // Luau raises "not enough memory", which pcall can catch
    void* p = realloc(ptr, nsize);
    if (!p)
        return NULL;
    g_used = g_used - osize + nsize;
    return p;
}

// interrupt runs at Luau's safepoints (loop back edges, calls). Once the deadline or the budget is
// passed every later safepoint raises again, so a script cannot pcall its way past it.
void interrupt(lua_State* L, int gc)
{
    if (gc >= 0)
        return; // never raise inside the collector
    if (g_trip == kNone)
    {
        g_count++;
        if (g_budget > 0 && g_count > g_budget)
            g_trip = kBudget;
        else if ((g_count & 255) == 0 && now_ns() > g_deadline)
            g_trip = kTimeout;
    }
    if (g_trip == kTimeout)
        luaL_error(L, "script timed out");
    if (g_trip == kBudget)
        luaL_error(L, "script exceeded its instruction budget");
}

// ---- JSON ------------------------------------------------------------------------------------

void push_null(lua_State* L)
{
    lua_getfield(L, LUA_REGISTRYINDEX, kNullKey);
}

bool is_null(lua_State* L, int idx)
{
    if (!lua_istable(L, idx))
        return false;
    idx = lua_absindex(L, idx);
    push_null(L);
    bool same = lua_rawequal(L, idx, -1);
    lua_pop(L, 1);
    return same;
}

void put_string(std::string& out, const char* s, size_t n)
{
    static const char* hex = "0123456789abcdef";
    out.push_back('"');
    for (size_t i = 0; i < n; i++)
    {
        unsigned char c = (unsigned char)s[i];
        switch (c)
        {
        case '"':
            out += "\\\"";
            break;
        case '\\':
            out += "\\\\";
            break;
        case '\n':
            out += "\\n";
            break;
        case '\r':
            out += "\\r";
            break;
        case '\t':
            out += "\\t";
            break;
        default:
            if (c < 0x20 || c == 0x7f)
            {
                out += "\\u00";
                out.push_back(hex[c >> 4]);
                out.push_back(hex[c & 15]);
            }
            else
                out.push_back((char)c);
        }
    }
    out.push_back('"');
}

void put_number(lua_State* L, std::string& out, double d)
{
    if (!isfinite(d))
        luaL_error(L, "json: cannot encode %s", isnan(d) ? "NaN" : "infinity");
    char buf[40];
    if (d == floor(d) && fabs(d) < 1e15)
        snprintf(buf, sizeof buf, "%lld", (long long)d);
    else
        snprintf(buf, sizeof buf, "%.17g", d);
    out += buf;
}

// encode appends the value at idx as JSON. Tables are read raw (no metamethod runs): a table whose
// keys are exactly 1..n is an array, any other non-empty table an object with string (or number)
// keys, and the empty table {}.
void encode(lua_State* L, int idx, int depth, std::string& out)
{
    if (out.size() > kMaxJSON)
        luaL_error(L, "json: value too large");
    idx = lua_absindex(L, idx);
    switch (lua_type(L, idx))
    {
    case LUA_TNIL:
        out += "null";
        return;
    case LUA_TBOOLEAN:
        out += lua_toboolean(L, idx) ? "true" : "false";
        return;
    case LUA_TNUMBER:
        put_number(L, out, lua_tonumber(L, idx));
        return;
    case LUA_TSTRING:
    {
        size_t n;
        const char* s = lua_tolstring(L, idx, &n);
        put_string(out, s, n);
        return;
    }
    case LUA_TBUFFER:
    {
        size_t n;
        const char* s = (const char*)lua_tobuffer(L, idx, &n);
        put_string(out, s, n);
        return;
    }
    case LUA_TTABLE:
        break;
    default:
        luaL_error(L, "json: cannot encode a %s", luaL_typename(L, idx));
    }
    if (is_null(L, idx))
    {
        out += "null";
        return;
    }
    if (depth >= kMaxDepth)
        luaL_error(L, "json: nested too deep (or a cycle)");
    luaL_checkstack(L, 4, "json");
    int n = lua_objlen(L, idx);
    int count = 0;
    bool array = n > 0;
    lua_pushnil(L);
    while (lua_next(L, idx) != 0)
    {
        count++;
        if (array)
        {
            double k = lua_type(L, -2) == LUA_TNUMBER ? lua_tonumber(L, -2) : 0;
            if (k < 1 || k > n || k != floor(k))
                array = false;
        }
        lua_pop(L, 1);
    }
    if (array && count == n)
    {
        out.push_back('[');
        for (int i = 1; i <= n; i++)
        {
            if (i > 1)
                out.push_back(',');
            lua_rawgeti(L, idx, i);
            encode(L, -1, depth + 1, out);
            lua_pop(L, 1);
        }
        out.push_back(']');
        return;
    }
    out.push_back('{');
    bool first = true;
    lua_pushnil(L);
    while (lua_next(L, idx) != 0)
    {
        if (!first)
            out.push_back(',');
        first = false;
        if (lua_type(L, -2) == LUA_TSTRING)
        {
            size_t kn;
            const char* k = lua_tolstring(L, -2, &kn);
            put_string(out, k, kn);
        }
        else if (lua_type(L, -2) == LUA_TNUMBER)
        {
            std::string k;
            put_number(L, k, lua_tonumber(L, -2));
            put_string(out, k.data(), k.size());
        }
        else
            luaL_error(L, "json: object keys must be strings or numbers, not %s", luaL_typename(L, -2));
        out.push_back(':');
        encode(L, -1, depth + 1, out);
        lua_pop(L, 1);
    }
    out.push_back('}');
}

struct Decoder
{
    lua_State* L;
    const char* p;
    const char* end;

    [[noreturn]] void fail(const char* what)
    {
        luaL_error(L, "json: %s at byte %d", what, (int)(p - start));
    }
    const char* start;

    void ws()
    {
        while (p < end && (*p == ' ' || *p == '\t' || *p == '\n' || *p == '\r'))
            p++;
    }

    bool lit(const char* s)
    {
        size_t n = strlen(s);
        if ((size_t)(end - p) >= n && memcmp(p, s, n) == 0)
        {
            p += n;
            return true;
        }
        return false;
    }

    static void utf8(std::string& s, uint32_t c)
    {
        if (c < 0x80)
            s.push_back((char)c);
        else if (c < 0x800)
        {
            s.push_back((char)(0xc0 | (c >> 6)));
            s.push_back((char)(0x80 | (c & 0x3f)));
        }
        else if (c < 0x10000)
        {
            s.push_back((char)(0xe0 | (c >> 12)));
            s.push_back((char)(0x80 | ((c >> 6) & 0x3f)));
            s.push_back((char)(0x80 | (c & 0x3f)));
        }
        else
        {
            s.push_back((char)(0xf0 | (c >> 18)));
            s.push_back((char)(0x80 | ((c >> 12) & 0x3f)));
            s.push_back((char)(0x80 | ((c >> 6) & 0x3f)));
            s.push_back((char)(0x80 | (c & 0x3f)));
        }
    }

    uint32_t hex4()
    {
        if (end - p < 4)
            fail("bad \\u escape");
        uint32_t v = 0;
        for (int i = 0; i < 4; i++)
        {
            char c = *p++;
            v <<= 4;
            if (c >= '0' && c <= '9')
                v |= c - '0';
            else if (c >= 'a' && c <= 'f')
                v |= c - 'a' + 10;
            else if (c >= 'A' && c <= 'F')
                v |= c - 'A' + 10;
            else
                fail("bad \\u escape");
        }
        return v;
    }

    void str()
    {
        p++; // "
        std::string s;
        for (;;)
        {
            if (p >= end)
                fail("unterminated string");
            char c = *p++;
            if (c == '"')
                break;
            if ((unsigned char)c < 0x20)
                fail("control character in string");
            if (c != '\\')
            {
                s.push_back(c);
                continue;
            }
            if (p >= end)
                fail("unterminated string");
            c = *p++;
            switch (c)
            {
            case '"':
            case '\\':
            case '/':
                s.push_back(c);
                break;
            case 'b':
                s.push_back('\b');
                break;
            case 'f':
                s.push_back('\f');
                break;
            case 'n':
                s.push_back('\n');
                break;
            case 'r':
                s.push_back('\r');
                break;
            case 't':
                s.push_back('\t');
                break;
            case 'u':
            {
                uint32_t cp = hex4();
                if (cp >= 0xd800 && cp < 0xdc00 && end - p >= 6 && p[0] == '\\' && p[1] == 'u')
                {
                    const char* save = p;
                    p += 2;
                    uint32_t lo = hex4();
                    if (lo >= 0xdc00 && lo < 0xe000)
                        cp = 0x10000 + ((cp - 0xd800) << 10) + (lo - 0xdc00);
                    else
                    {
                        p = save;
                        cp = 0xfffd;
                    }
                }
                else if (cp >= 0xd800 && cp < 0xe000)
                    cp = 0xfffd;
                utf8(s, cp);
                break;
            }
            default:
                fail("bad escape");
            }
        }
        lua_pushlstring(L, s.data(), s.size());
    }

    void num()
    {
        const char* b = p;
        if (p < end && *p == '-')
            p++;
        while (p < end && ((*p >= '0' && *p <= '9') || *p == '.' || *p == 'e' || *p == 'E' || *p == '+' || *p == '-'))
            p++;
        std::string t(b, p - b);
        char* e = NULL;
        double d = strtod(t.c_str(), &e);
        if (t.empty() || e != t.c_str() + t.size())
            fail("bad number");
        lua_pushnumber(L, d);
    }

    // value pushes one value; JSON null is nil in objects and arrays (as Roblox's JSONDecode).
    void value(int depth)
    {
        if (depth > kMaxDepth)
            fail("nested too deep");
        luaL_checkstack(L, 3, "json");
        ws();
        if (p >= end)
            fail("unexpected end");
        switch (*p)
        {
        case '{':
        {
            p++;
            lua_newtable(L);
            ws();
            if (p < end && *p == '}')
            {
                p++;
                return;
            }
            for (;;)
            {
                ws();
                if (p >= end || *p != '"')
                    fail("expected a key");
                str();
                ws();
                if (p >= end || *p != ':')
                    fail("expected ':'");
                p++;
                value(depth + 1);
                lua_rawset(L, -3);
                ws();
                if (p < end && *p == ',')
                {
                    p++;
                    continue;
                }
                if (p < end && *p == '}')
                {
                    p++;
                    return;
                }
                fail("expected ',' or '}'");
            }
        }
        case '[':
        {
            p++;
            lua_newtable(L);
            ws();
            if (p < end && *p == ']')
            {
                p++;
                return;
            }
            for (int i = 1;; i++)
            {
                value(depth + 1);
                lua_rawseti(L, -2, i);
                ws();
                if (p < end && *p == ',')
                {
                    p++;
                    continue;
                }
                if (p < end && *p == ']')
                {
                    p++;
                    return;
                }
                fail("expected ',' or ']'");
            }
        }
        case '"':
            str();
            return;
        case 't':
            if (lit("true"))
            {
                lua_pushboolean(L, 1);
                return;
            }
            break;
        case 'f':
            if (lit("false"))
            {
                lua_pushboolean(L, 0);
                return;
            }
            break;
        case 'n':
            if (lit("null"))
            {
                lua_pushnil(L);
                return;
            }
            break;
        default:
            if (*p == '-' || (*p >= '0' && *p <= '9'))
            {
                num();
                return;
            }
        }
        fail("unexpected character");
    }
};

void decode(lua_State* L, const char* s, size_t n)
{
    Decoder d{L, s, s + n, s};
    d.value(0);
    d.ws();
    if (d.p != d.end)
        d.fail("trailing data");
}

int l_json_encode(lua_State* L)
{
    luaL_checkany(L, 1);
    std::string out;
    encode(L, 1, 0, out);
    lua_pushlstring(L, out.data(), out.size());
    return 1;
}

int l_json_decode(lua_State* L)
{
    size_t n;
    const char* s = luaL_checklstring(L, 1, &n);
    decode(L, s, n);
    return 1;
}

// ---- the host ----------------------------------------------------------------------------------

// call_host sends name and the JSON in to the host and pushes the decoded answer's "v", or raises
// "<code>: <msg>" for a refusal.
int call_host(lua_State* L, const char* name, size_t name_len, const std::string& in)
{
    int32_t h = host_call(name, (int32_t)name_len, in.data(), (int32_t)in.size());
    if (h < 0)
        luaL_error(L, "%s: the host refused the call (%d)", name, (int)h);
    int32_t size = host_result(h, NULL, 0);
    if (size < 0)
        luaL_error(L, "%s: no result", name);
    std::string buf((size_t)size, '\0');
    if (size > 0 && host_result(h, &buf[0], size) != size)
        luaL_error(L, "%s: result lost", name);
    decode(L, buf.data(), buf.size());
    if (!lua_istable(L, -1))
        luaL_error(L, "%s: bad answer from the host", name);
    lua_getfield(L, -1, "ok");
    bool ok = lua_toboolean(L, -1);
    lua_pop(L, 1);
    if (!ok)
    {
        lua_getfield(L, -1, "err");
        const char* code = "error";
        const char* msg = "";
        if (lua_istable(L, -1))
        {
            lua_getfield(L, -1, "code");
            lua_getfield(L, -2, "msg");
            if (lua_isstring(L, -2))
                code = lua_tostring(L, -2);
            if (lua_isstring(L, -1))
                msg = lua_tostring(L, -1);
        }
        std::string text = std::string(code) + ": " + msg;
        luaL_error(L, "%s", text.c_str());
    }
    lua_getfield(L, -1, "v");
    return 1;
}

// l_binding is one registry binding: upvalue 1 its name, 2 its parameter names (a table), 3 true
// when its first argument is the whole argument object (http.request{…}).
int l_binding(lua_State* L)
{
    int nargs = lua_gettop(L);
    size_t name_len;
    const char* name = lua_tolstring(L, lua_upvalueindex(1), &name_len);
    bool spread = lua_toboolean(L, lua_upvalueindex(3));
    std::string in;
    if (spread)
    {
        if (nargs < 1 || lua_isnil(L, 1))
            in = "{}";
        else
        {
            luaL_checktype(L, 1, LUA_TTABLE);
            encode(L, 1, 0, in);
        }
    }
    else
    {
        lua_newtable(L);
        int obj = lua_gettop(L);
        int np = lua_objlen(L, lua_upvalueindex(2));
        for (int i = 1; i <= np && i <= nargs; i++)
        {
            lua_rawgeti(L, lua_upvalueindex(2), i);
            lua_pushvalue(L, i);
            lua_rawset(L, obj);
        }
        encode(L, obj, 0, in);
        if (in == "[]")
            in = "{}";
    }
    return call_host(L, name, name_len, in);
}

int l_null_tostring(lua_State* L)
{
    lua_pushliteral(L, "null");
    return 1;
}

int l_collectgarbage(lua_State* L)
{
    const char* opt = luaL_optstring(L, 1, "count");
    if (strcmp(opt, "count") != 0)
        luaL_error(L, "collectgarbage: only \"count\" is allowed");
    lua_pushnumber(L, lua_gc(L, LUA_GCCOUNT, 0) + lua_gc(L, LUA_GCCOUNTB, 0) / 1024.0);
    return 1;
}

// os.time() is the host's wall clock (the module has none of its own); os.time(t) converts a date
// table with Luau's own os.time (upvalue 1), which reads no clock.
int l_os_time(lua_State* L)
{
    if (lua_isnoneornil(L, 1))
    {
        static const char name[] = "time.now";
        call_host(L, name, sizeof name - 1, "{}");
        lua_pushnumber(L, floor(lua_tonumber(L, -1)));
        return 1;
    }
    lua_pushvalue(L, lua_upvalueindex(1));
    lua_pushvalue(L, 1);
    lua_call(L, 1, 1);
    return 1;
}

void open_lib(lua_State* L, lua_CFunction f, const char* name)
{
    lua_pushcfunction(L, f, NULL);
    lua_pushstring(L, name);
    lua_call(L, 1, 0);
}

// subtable returns (pushes) t[key], making it a table when absent; t is at idx.
void subtable(lua_State* L, int idx, const char* key, size_t n)
{
    idx = lua_absindex(L, idx);
    lua_pushlstring(L, key, n);
    lua_rawget(L, idx);
    if (lua_istable(L, -1))
        return;
    lua_pop(L, 1);
    lua_newtable(L);
    lua_pushlstring(L, key, n);
    lua_pushvalue(L, -2);
    lua_rawset(L, idx);
}

void freeze(lua_State* L, int idx, int depth)
{
    idx = lua_absindex(L, idx);
    if (depth > 8)
        return;
    lua_pushnil(L);
    while (lua_next(L, idx) != 0)
    {
        if (lua_istable(L, -1) && !lua_getreadonly(L, -1))
            freeze(L, -1, depth + 1);
        lua_pop(L, 1);
    }
    lua_setreadonly(L, idx, true);
}

// install puts nt (and the global aliases) from the spec's bindings (table at idx) into globals.
void install(lua_State* L, int bindings)
{
    bindings = lua_absindex(L, bindings);
    lua_newtable(L);
    int nt = lua_gettop(L);
    int n = lua_objlen(L, bindings);
    for (int i = 1; i <= n; i++)
    {
        lua_rawgeti(L, bindings, i);
        int b = lua_gettop(L);
        lua_getfield(L, b, "name");
        size_t name_len;
        const char* name = lua_tolstring(L, -1, &name_len);
        if (!name || name_len == 0 || name_len > 64)
            luaL_error(L, "bad binding");
        bool local = (lua_getfield(L, b, "local"), lua_toboolean(L, -1));
        lua_pop(L, 1);
        if (local)
        {
            if (strcmp(name, "json.encode") == 0)
                lua_pushcfunction(L, l_json_encode, "json.encode");
            else if (strcmp(name, "json.decode") == 0)
                lua_pushcfunction(L, l_json_decode, "json.decode");
            else
                luaL_error(L, "unknown local binding %s", name);
        }
        else
        {
            lua_pushvalue(L, -1); // name
            lua_getfield(L, b, "params");
            if (!lua_istable(L, -1))
            {
                lua_pop(L, 1);
                lua_newtable(L);
            }
            lua_getfield(L, b, "spread");
            lua_pushcclosure(L, l_binding, name, 3);
        }
        int fn = lua_gettop(L);
        // nt.<a>.<b>…: every segment but the last is a table
        lua_pushvalue(L, nt);
        const char* seg = name;
        for (;;)
        {
            const char* dot = strchr(seg, '.');
            if (!dot)
                break;
            subtable(L, -1, seg, dot - seg);
            lua_remove(L, -2);
            seg = dot + 1;
        }
        lua_pushstring(L, seg);
        lua_pushvalue(L, fn);
        lua_rawset(L, -3);
        lua_pop(L, 1);
        lua_getfield(L, b, "global");
        if (lua_isstring(L, -1) && lua_objlen(L, -1) > 0)
        {
            lua_pushvalue(L, fn);
            lua_setglobal(L, lua_tostring(L, -2));
        }
        lua_settop(L, b - 1);
    }
    // nt.json.null: a frozen sentinel that encodes as null
    subtable(L, nt, "json", 4);
    lua_pushstring(L, "null");
    push_null(L);
    lua_rawset(L, -3);
    lua_pop(L, 1);
    lua_setglobal(L, "nt");
}

// message is the error at idx as text, without running any metamethod.
std::string message(lua_State* L, int idx)
{
    if (lua_type(L, idx) == LUA_TSTRING || lua_type(L, idx) == LUA_TNUMBER)
    {
        size_t n;
        const char* s = lua_tolstring(L, idx, &n);
        return std::string(s, n);
    }
    return std::string("(error object is a ") + luaL_typename(L, idx) + " value)";
}

void set_error(const char* code, const std::string& msg)
{
    g_out = "{\"ok\":false,\"err\":{\"code\":";
    put_string(g_out, code, strlen(code));
    g_out += ",\"msg\":";
    put_string(g_out, msg.data(), msg.size() > 4000 ? 4000 : msg.size());
    g_out += "}}";
}

// error_code names how a failed call ended.
const char* error_code(int status)
{
    if (g_trip == kTimeout)
        return "timeout";
    if (g_trip == kBudget)
        return "budget";
    if (status == LUA_ERRMEM)
        return "memory";
    return "runtime";
}

char* compile(const char* src, size_t len, size_t* outsize)
{
    FInt::LuauRecursionLimit.value = kRecursionLimit;
    lua_CompileOptions opts = {};
    opts.optimizationLevel = 1;
    opts.debugLevel = 1;
    return luau_compile(src, len, &opts, outsize);
}

// guarded runs the remaining steps of a run in a protected call, so every Luau error (and every
// error of ours raised with luaL_error) ends here.
struct RunCtx
{
    const char* src;
    size_t len;
    const char* spec;
    size_t spec_len;
};

int protected_run(lua_State* L)
{
    RunCtx* rc = (RunCtx*)lua_touserdata(L, 1);
    lua_settop(L, 0);
    decode(L, rc->spec, rc->spec_len); // 1: spec
    if (!lua_istable(L, 1))
        luaL_error(L, "bad spec");

    // nt.json.null
    lua_newtable(L);
    lua_newtable(L);
    lua_pushcfunction(L, l_null_tostring, "tostring");
    lua_setfield(L, -2, "__tostring");
    lua_setreadonly(L, -1, true);
    lua_setmetatable(L, -2);
    lua_setreadonly(L, -1, true);
    lua_setfield(L, LUA_REGISTRYINDEX, kNullKey);

    open_lib(L, luaopen_base, "");
    open_lib(L, luaopen_string, LUA_STRLIBNAME);
    open_lib(L, luaopen_table, LUA_TABLIBNAME);
    open_lib(L, luaopen_math, LUA_MATHLIBNAME);
    open_lib(L, luaopen_utf8, LUA_UTF8LIBNAME);
    open_lib(L, luaopen_bit32, LUA_BITLIBNAME);
    open_lib(L, luaopen_buffer, LUA_BUFFERLIBNAME);
    for (const char* g : {"getfenv", "setfenv", "newproxy", "gcinfo", "loadstring", "load", "require", "debug", "dofile", "loadfile"})
    {
        lua_pushnil(L);
        lua_setglobal(L, g);
    }
    lua_pushcfunction(L, l_collectgarbage, "collectgarbage");
    lua_setglobal(L, "collectgarbage");
    // os: clock and time only (os.date, os.getenv and the rest are not opened)
    open_lib(L, luaopen_os, LUA_OSLIBNAME);
    lua_getglobal(L, "os");
    lua_newtable(L);
    lua_getfield(L, -2, "clock");
    lua_setfield(L, -2, "clock");
    lua_getfield(L, -2, "time");
    lua_pushcclosure(L, l_os_time, "time", 1);
    lua_setfield(L, -2, "time");
    lua_setglobal(L, "os");
    lua_pop(L, 1);

    lua_getfield(L, 1, "bindings");
    if (lua_istable(L, -1))
        install(L, -1);
    lua_pop(L, 1);
    lua_getglobal(L, "nt");
    if (lua_istable(L, -1))
    {
        lua_getfield(L, 1, "args");
        lua_setfield(L, -2, "args");
        // nt and its tables are frozen, but not nt.args (the script's own copy of its input)
        lua_pushnil(L);
        while (lua_next(L, -2) != 0)
        {
            bool skip = lua_type(L, -2) == LUA_TSTRING && strcmp(lua_tostring(L, -2), "args") == 0;
            if (lua_istable(L, -1) && !skip)
                freeze(L, -1, 0);
            lua_pop(L, 1);
        }
        lua_setreadonly(L, -1, true);
    }
    lua_pop(L, 1);
    luaL_sandbox(L);

    lua_State* T = lua_newthread(L);
    luaL_sandboxthread(T);
    lua_callbacks(L)->interrupt = interrupt;

    size_t bcsize = 0;
    char* bc = compile(rc->src, rc->len, &bcsize);
    if (!bc)
        luaL_error(L, "compile: out of memory");
    std::string bytecode(bc, bcsize);
    free(bc);
    lua_getfield(L, 1, "name");
    std::string chunk = std::string("=") + (lua_isstring(L, -1) ? lua_tostring(L, -1) : "script");
    lua_pop(L, 1);
    if (luau_load(T, chunk.c_str(), bytecode.data(), bytecode.size(), 0) != 0)
    {
        std::string m = message(T, -1);
        set_error("syntax", m);
        lua_pushboolean(L, 0);
        return 1;
    }
    int status = lua_pcall(T, 0, 1, 0);
    if (status == 0)
    {
        // The entry: the named function (of the chunk's returned table, else a global the script
        // defined), or the function the chunk returned; a chunk that returns anything else is
        // the result itself.
        lua_getfield(L, 1, "entry");
        std::string entry = lua_isstring(L, -1) ? lua_tostring(L, -1) : "";
        lua_pop(L, 1);
        bool call = false;
        if (!entry.empty())
        {
            if (lua_istable(T, -1))
            {
                lua_pushlstring(T, entry.data(), entry.size());
                lua_rawget(T, -2);
                lua_remove(T, -2);
            }
            else
            {
                lua_pop(T, 1);
                lua_pushlstring(T, entry.data(), entry.size());
                lua_rawget(T, LUA_GLOBALSINDEX);
            }
            if (!lua_isfunction(T, -1))
            {
                set_error("entry", "the script defines no function " + entry);
                lua_pushboolean(L, 0);
                return 1;
            }
            call = true;
        }
        else
            call = lua_isfunction(T, -1);
        if (call)
        {
            lua_getglobal(T, "nt");
            if (lua_istable(T, -1))
                lua_getfield(T, -1, "args");
            else
                lua_pushnil(T);
            lua_remove(T, -2);
            status = lua_pcall(T, 1, 1, 0);
        }
    }
    if (status != 0)
    {
        set_error(error_code(status), message(T, -1));
        lua_pushboolean(L, 0);
        return 1;
    }
    // encoded on L, so an error (NaN, a function) is raised where the run's pcall reads it
    lua_xmove(T, L, 1);
    std::string out = "{\"ok\":true,\"v\":";
    encode(L, -1, 0, out);
    out += "}";
    g_out.swap(out);
    lua_pushboolean(L, 1);
    return 1;
}

} // namespace

extern "C" {

__attribute__((export_name("nt_alloc"))) void* nt_alloc(int32_t n)
{
    return n > 0 ? malloc((size_t)n) : NULL;
}

__attribute__((export_name("nt_free"))) void nt_free(void* p)
{
    free(p);
}

__attribute__((export_name("nt_out_ptr"))) const char* nt_out_ptr()
{
    return g_out.data();
}

__attribute__((export_name("nt_out_len"))) int32_t nt_out_len()
{
    return (int32_t)g_out.size();
}

// nt_check compiles src; on an error the output is the message.
__attribute__((export_name("nt_check"))) int32_t nt_check(const char* src, int32_t len)
{
    try
    {
        if (len < 0 || (size_t)len > kMaxSource)
        {
            g_out = "source too large";
            return 1;
        }
        size_t size = 0;
        char* bc = compile(src, (size_t)len, &size);
        if (!bc)
        {
            g_out = "out of memory";
            return 1;
        }
        int bad = size > 0 && bc[0] == 0;
        g_out = bad ? std::string(bc + 1, size - 1) : std::string();
        free(bc);
        return bad;
    }
    catch (...)
    {
        g_out = "the compiler failed";
        return 1;
    }
}

__attribute__((export_name("nt_run"))) int32_t nt_run(const char* src, int32_t len, const char* spec, int32_t spec_len,
                                                      int32_t mem_cap, int32_t timeout_ms, int64_t budget)
{
    try
    {
        if (len < 0 || (size_t)len > kMaxSource)
        {
            set_error("too_large", "source too large");
            return 1;
        }
        g_used = 0;
        g_cap = mem_cap > 0 ? (size_t)mem_cap : (48u << 20);
        g_budget = budget;
        g_count = 0;
        g_trip = kNone;
        g_deadline = now_ns() + (int64_t)(timeout_ms > 0 ? timeout_ms : 30000) * 1000000LL;
        lua_State* L = lua_newstate(counting_alloc, NULL);
        if (!L)
        {
            set_error("memory", "no memory for a state");
            return 1;
        }
        RunCtx rc{src, (size_t)len, spec, (size_t)(spec_len > 0 ? spec_len : 0)};
        lua_pushcfunction(L, protected_run, "run");
        lua_pushlightuserdata(L, &rc);
        int status = lua_pcall(L, 1, 1, 0);
        int ok = status == 0 && lua_toboolean(L, -1);
        if (status != 0)
            set_error(error_code(status), message(L, -1));
        fflush(stdout);
        fflush(stderr);
        // The state is not closed: the instance is thrown away after one run.
        return ok ? 0 : 1;
    }
    catch (const std::bad_alloc&)
    {
        set_error("memory", "out of memory");
        return 1;
    }
    catch (...)
    {
        set_error("runtime", "the runtime failed");
        return 1;
    }
}
}
