// Synthetic owner-review contract tests. No server, credentials or live writes.
const { test } = require('node:test');
const assert = require('node:assert/strict');
const fs = require('node:fs');
const vm = require('node:vm');
const source = fs.readFileSync(require('node:path').join(__dirname, '../review.js'), 'utf8');
const handlers = {};
const context = vm.createContext({ console, Map, TextEncoder, setTimeout, clearTimeout, setInterval() {}, document: { querySelector: () => ({ addEventListener: (type, fn) => { (handlers[type] ??= []).push(fn); } }), addEventListener() {}, visibilityState: 'hidden' }, location: {hash:'', pathname:'/'}, sessionStorage: {getItem: () => ''}, history: {}, fetch: async () => ({ok: true,json: async()=>({agents:[]})}) });
vm.runInContext(source.replace(/^load\(\);$/m, ''), context);
test('patch preview escapes model text and binds exact revisions', () => {
  const result = vm.runInContext(`skillLearningHTML('a', {patches:[{id:'p',name:'skill',status:'pending',before:'<script>bad</script>',after:'replacement',expectedRevision:'base-r',currentRevision:'live-r',audience:'agent',canApprove:true,canRollback:true}],maintenance:[{name:'old',action:'archive_review',reason:'inactive',revision:'old-r'}],archives:[{id:'ar',name:'old',status:'archived'}]}, false)`, context);
  assert.ok(result.includes('&lt;script&gt;bad&lt;/script&gt;'));
  assert.match(result, /data-learning-action="approve_patch"[^>]+data-revision="base-r"/);
  assert.match(result, /data-learning-action="rollback_patch"[^>]+data-revision="live-r"/);
  assert.match(result, /data-learning-action="archive_skill"[^>]+data-revision="old-r"/);
  assert.match(result, /data-learning-action="restore_skill"[^>]+data-target-id="ar"/);
});
test('mutation disables new controls; background budgets are labeled home-wide', () => {
 const result = vm.runInContext(`backgroundLearningHTML('a', {enabled:true,callsUsed:2,callsLimit:8,tokensReserved:1024,tokensLimit:4096,pending:1,completed:0,failed:1,interrupted:0,jobs:[{id:'j',kind:'correction',state:'failed',attempts:1,ownerRetries:0,resultCode:'provider'}],candidates:[]}, true)`, context);
 assert.match(result, /Home budget, rolling 24 hours/);
 assert.match(result, /data-learning-action="retry_job"[^>]+disabled/);
 assert.match(result, /remain proposals/);
});
