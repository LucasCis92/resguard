// Package version expone la versión del binario, inyectada en tiempo de compilación:
//
//	go build -ldflags "-X github.com/LucasCis92/resguard/internal/version.Version=v0.1.0"
package version

// Version del binario. Vale "dev" si no se inyecta al compilar.
var Version = "dev"
