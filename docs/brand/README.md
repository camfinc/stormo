# Stormo brand

The mark is a swarm of agents curling into one storm: dots on a spiral, evenly spaced, growing
and cooling from teal at the calm centre to indigo at the edge. The wordmark is monoline paths, so
nothing depends on a font.

| file | use |
|---|---|
| `logo-light.svg` · `logo-dark.svg` | mark + wordmark, for light and dark backgrounds (the README header) |
| `mark.svg` | the mark on its dark tile: app icons, avatars |
| `mark-bare.svg` | the mark alone, on any background |
| `favicon.svg` | a nine-dot cut of the mark that stays legible at 16 px (the office's browser tab) |
| `wordmark-light.svg` · `wordmark-dark.svg` | the name alone |
| `social-preview.png` (`.svg` source) | GitHub's social preview, 1280×640 |
| `apps/macos/Stormo/AppIcon.icon` | the macOS app icon: an Icon Composer document, the tile as its fill and each dot an opaque glass layer |

Colours: teal `#2dd4bf` → indigo `#818cf8`, tile `#0d141c`, light ink `#e8eef5`, dark ink `#0d141c`
(the office UI's palette). Leave clear space of at least one dot's width around the mark, and never
recolour the dots individually or redraw the spiral by hand: the files are generated from one
spiral (even spacing along an Archimedean curve), so edits go to the generator and every file is
regenerated together:

```sh
go run ./tools/brandgen        # rewrites docs/brand/*.svg and the app icon
```

`social-preview.png` is a browser render of `social-preview.svg` at 1280×640 (for example
`chrome --headless --window-size=1280,640 --screenshot=social-preview.png social-preview.svg`),
uploaded in the repository's Settings → Social preview.
