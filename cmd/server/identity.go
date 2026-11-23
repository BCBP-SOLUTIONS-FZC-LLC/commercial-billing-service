package main

import "github.com/BCBP-SOLUTIONS-FZC-LLC/commercial-billing-service/internal/config"

// loggerEnv maps APP_ENV onto the value gincommon's logger understands:
// development output for local/dev/test, production JSON otherwise. The one
// resolved environment (config.Config.AppEnv) is passed explicitly to
// metrics and tracing as well, so all three agree (§12, §3 identity.go).
func loggerEnv(appEnv string) string {
	if config.IsDevLike(appEnv) {
		return "dev"
	}
	return appEnv
}
