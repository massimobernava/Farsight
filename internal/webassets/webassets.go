// Package webassets holds static assets shared by more than one Farsight
// binary — today just the favicon, embedded once here instead of
// duplicated per binary, so there's a single file to update.
package webassets

import _ "embed"

//go:embed favicon.ico
var FaviconICO []byte
