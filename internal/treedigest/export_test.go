package treedigest

import "io/fs"

func TreeOf(fsys fs.FS) (string, error) { return treeOf(fsys) }

type RawFS = rawFS
