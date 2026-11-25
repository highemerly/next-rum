// Package rumcollector is the module root. It embeds the client-side assets
// (beacon.js, web-vitals.js and its attribution build, and the default
// rum-config.json) into the binary so the collector can serve them without
// external files, matching the eew-fedibot / GoSorry deployment style.
package rumcollector

import _ "embed"

//go:embed assets/beacon.js
var BeaconJS []byte

//go:embed assets/web-vitals.js
var WebVitalsJS []byte

// WebVitalsAttributionJS is the attribution build of the same web-vitals
// release, loaded instead of WebVitalsJS by services with the attribution
// feature (it is larger, so the others keep the plain build).
//
//go:embed assets/web-vitals.attribution.js
var WebVitalsAttributionJS []byte

//go:embed assets/rum-config.json
var DefaultConfigJSON []byte
