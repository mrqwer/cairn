package adapter

import "embed"

//go:embed builtin/*.toml
var builtinFS embed.FS
