// Package version exposes the binary's version, injected at build time:
//
//	go build -ldflags "-X github.com/LucasCis92/resguard/internal/version.Version=v0.1.0"
package version

// Version of the binary; "dev" when not injected at build time.
var Version = "dev"
