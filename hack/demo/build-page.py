#!/usr/bin/env python3
"""Builds the web player page from page.html.

Inlines the stylesheet of the pinned asciinema-player (the script itself is
loaded from jsDelivr) and writes two variants next to the casts:

  <out>/index.html     a complete document (GitHub Pages, any static host)
  <out>/fragment.html  the page body only, for hosts that add the skeleton

    build-page.py page.html player.css <out>
"""

import os
import sys

page_file, css_file, out = sys.argv[1:4]
page = open(page_file).read().replace("{{PLAYER_CSS}}", open(css_file).read().strip())
open(os.path.join(out, "fragment.html"), "w").write(page)
open(os.path.join(out, "index.html"), "w").write(
    '<!doctype html>\n<html lang="en">\n<head>\n<meta charset="utf-8">\n'
    '<meta name="viewport" content="width=device-width, initial-scale=1, viewport-fit=cover">\n'
    "<style>body{margin:0}[hidden]{display:none!important}</style>\n</head>\n<body>\n"
    + page
    + "\n</body>\n</html>\n"
)
print(f"wrote {out}/index.html and {out}/fragment.html")
