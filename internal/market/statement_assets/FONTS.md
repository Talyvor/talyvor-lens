# The statement's fonts

Three files, cut from the two brand faces so a plain PDF writer can embed them as simple TrueType fonts.

| File | From | What changed |
|---|---|---|
| FigtreeStatement-Regular.ttf | Figtree, weight 400 | Tabular figures are the default, so money lines up in columns. Family name changed |
| FigtreeStatement-SemiBold.ttf | Figtree, weight 600 | The same |
| BricolageStatement-Bold.ttf | Bricolage Grotesque, weight 750, optical size 96, width 100 | Made static. Family name changed |

Licence: both faces are under the SIL Open Font License 1.1 (https://openfontlicense.org). It allows changing and embedding them. The copyright and licence records are kept inside each file: "Copyright 2022 The Figtree Project Authors (https://github.com/erikdkennedy/figtree)" and "Copyright 2022 The Bricolage Grotesque Project Authors (https://github.com/ateliertriay/bricolage)". Neither declares a reserved font name. The files may not be sold on their own.

`widths.json` holds, for each file, what a simple TrueType font needs in a PDF: the advance of every WinAnsi character from 32 to 255 in thousandths of an em, ascent, descent, cap height, bounding box, stem and flags. With it the writer needs no font parser: embed the file bytes as FontFile2, write the FontDescriptor from these numbers, and use WinAnsiEncoding.

`make_fonts.py` in the folder above made all of this from the variable fonts with fontTools.
