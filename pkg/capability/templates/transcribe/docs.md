Give one audio or video file from your shared space (`files: ["/shared/<layer>/…/call.mp3"]`; any
format ffmpeg reads: mp3, m4a, wav, ogg/opus voice notes, mp4, mov…). The first file is transcribed;
pass one file per job.

Answer: `{language, duration_seconds, text, segments: [{start, end, text}], truncated}`. `text` and
`segments` stop at about 200 KB (`truncated: true`); the transcript files hold everything. Files
written to your output folder: `<name>.txt`, `<name>.srt` by default (`formats` picks txt, srt, vtt,
json).

- `language: auto` detects the language from the first 30 seconds; name it when you know it
  (better on short clips and mixed speech).
- `prompt` takes the names and terms the audio uses, spelled as you want them.
- `translate: true` gives English text from any language.
- Long recordings take time on CPU (roughly a fifth to a half of their length): start them with
  cap_start and let the notice wake you, rather than waiting in cap_run.
