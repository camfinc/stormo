Generate one image per job from a `prompt`, with a current image model (`model`, default
`openai/gpt-image-2.5-flare`). Hand over images to guide it (`files: ["/shared/<layer>/…/frame.png"]`,
or an upload you were sent) and name the ones to use in `references` (all of them when left out):
the place, the light, the style or the subject to keep.

- `aspect_ratio` shapes it (9:16 for a story or reel, 16:9 for a thumbnail, 1:1, 3:4…); each model
  takes its own set and the nearest one is yours to pick.
- `quality` (low, medium, high) for GPT Image models; `resolution` (1K, 2K, 4K) for Nano Banana,
  Flux and Seedream. `background: transparent` (GPT Image) for a sticker or a cutout.
- Write the prompt as a brief: the subject, the place, the light, the composition (where to leave
  calm room for words added later), the style. Say "no text" when none is wanted: models spell
  badly, and words belong in a layout over the image.

Answer: `{files: [<name>.png], model, media_type, width, height, cost_usd}`; the picture is in your
`out` folder. Every job is paid on the instance's account: the same prompt to the same model gives
much the same picture, so change the prompt, the references or the model rather than rerunning.
Never ask for a real, recognisable person.
