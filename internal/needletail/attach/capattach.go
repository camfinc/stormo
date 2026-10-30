package attach

import (
	"context"
	"encoding/json"
	"fmt"
	"regexp"

	"github.com/camfinc/stormo/internal/needletail/messaging"
	"github.com/camfinc/stormo/internal/needletail/tools"
)

// AttachTool is cap_attach: one of this conversation's uploads (a video, a voice note…) sent to a
// capability job of the agent's that waits for it (cap_start files_pending), straight into the
// job's folder through the core: never through the shared space.
const AttachTool = "cap_attach"

// Uploader sends a file to one of the agent's waiting capability jobs (the core's
// POST /v1/capabilities/jobs/<id>/files) and gives the job as the core answers.
type Uploader func(ctx context.Context, job int64, name string, body []byte) (json.RawMessage, error)

var jobName = regexp.MustCompile(`^[A-Za-z0-9]`)

// AttachTool sends an upload to a waiting job. It is a write to the core: a marked upload marks
// the conversation first, and a tainted run puts the call to a person. consume drops the upload
// from the store once the core has it.
func (s *Store) AttachTool(up Uploader) tools.Tool {
	return tools.Tool{Name: AttachTool,
		Description: "Send a file you were sent in this conversation (a video, a voice note, an image…) to one of your capability jobs that waits for it " +
			"(cap_start with files_pending): straight into the job, never into the shared space. consume: true drops it from your files once it went.",
		Schema: json.RawMessage(`{"type":"object","properties":{` +
			`"job":{"type":"integer","description":"the job number cap_start gave"},` +
			`"file":{"type":"string","description":"the file's id (F…) or name (view_file lists them)"},` +
			`"consume":{"type":"boolean","description":"drop the file from your files once the job has it (default false)"}},"required":["job","file"]}`),
		Run: func(ctx context.Context, env *tools.Env, raw json.RawMessage) (any, error) {
			var a struct {
				Job     int64  `json:"job"`
				File    string `json:"file"`
				Consume bool   `json:"consume"`
			}
			if err := json.Unmarshal(raw, &a); err != nil || a.Job <= 0 || a.File == "" {
				return nil, fmt.Errorf("arguments: {job, file, consume?}")
			}
			conv := ""
			if env != nil {
				conv = env.Session
				if env.Conversation != nil {
					if c := env.Conversation(env.Session); c != "" {
						conv = c
					}
				}
			}
			e, err := s.Find(conv, a.File)
			if err != nil {
				return nil, err
			}
			if e.Marked {
				// its sender may not instruct the agent: what follows from it is decided as marked
				if err := messaging.MarkUntrusted(ctx, whyMarked); err != nil {
					return nil, fmt.Errorf("a marked file cannot be sent: %w", err)
				}
			}
			if why := tools.UntrustedWhy(ctx, env); why != "" {
				return nil, messaging.NeedsApproval(fmt.Sprintf("sending a file you were sent (%s) to capability job %d", e.Name, a.Job), why)
			}
			body, err := s.Read(e)
			if err != nil {
				return nil, err
			}
			name := SafeName(e.Name)
			if !jobName.MatchString(name) {
				name = "file-" + name
			}
			job, err := up(ctx, a.Job, name, body)
			if err != nil {
				return nil, err
			}
			out := map[string]any{"job": a.Job, "file": name, "bytes": len(body), "consumed": false, "job_state": job}
			if a.Consume {
				if err := s.Remove(conv, e.ID); err != nil {
					out["note"] = "sent, but it could not be dropped from your files: " + err.Error()
				} else {
					out["consumed"] = true
				}
			}
			return out, nil
		},
	}
}
