package version

// Version and Commit are set at build time via -ldflags:
//   -X github.com/ylallemant/synergia/internal/client/version.Version=x.y.z
//   -X github.com/ylallemant/synergia/internal/client/version.Commit=abc1234
var Version = "0.1.0-dev"
var Commit  = "unknown"
