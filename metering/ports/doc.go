// Package ports declares the interfaces at the edges of the hexagon.
//
// driving.go is how the world calls metering (Meter, LedgerWriter); driven.go is what metering
// needs from the world (stores, a bus, a clock). The app depends on these and on domain, never on
// an infrastructure SDK; an adapter implements a driven port and never imports the app.
package ports
