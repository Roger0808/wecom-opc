package web

import "embed"

//go:embed admin.html nav.html *.js
var AdminFS embed.FS
