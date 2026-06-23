// Package rumcollector is the module root. It embeds the client-side assets
// (beacon.js, web-vitals.js, and the default rum-config.json) into the binary so
// the collector can serve them without external files, matching the eew-fedibot /
// GoSorry deployment style.
package rumcollector

import _ "embed"

//go:embed assets/beacon.js
var BeaconJS []byte

//go:embed assets/web-vitals.js
var WebVitalsJS []byte

//go:embed assets/rum-config.json
var DefaultConfigJSON []byte
