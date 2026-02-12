package deno

import (
	"strings"

	"github.com/microsoft/typescript-go/internal/tspath"
)

func IsTypesNodePkgPath(path tspath.Path) bool {
	return strings.HasSuffix(string(path), ".d.ts") && strings.Contains(string(path), "/@types/node/")
}
