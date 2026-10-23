module github.com/talyvor/lens

go 1.25.0

toolchain go1.26.9 // the version this repo SHIPS: setup-go pins below it are upgraded to it, so the released binary is built with exactly this. >= 1.26.9 clears GO-2026-{6617,6613,6612,6611,6610,6608,6607,6605,6603,6600,6599}, eleven reachable stdlib advisories (net/http, net/textproto, crypto/tls, html/template) that made the govulncheck gate red for every PR; >= 1.26.6 cleared GO-2026-{6218,6091,6090,6089,6088,5972,5026}, seven reachable stdlib advisories that made the govulncheck gate red on main; >= 1.26.5 was for GO-2026-5856 (crypto/tls ECH privacy leak). Enforced by the govulncheck CI gate, and kept in lockstep with ci.yaml's graders by internal/toolchainaudit

require (
	github.com/alicebob/miniredis/v2 v2.38.0
	github.com/dop251/goja v0.0.0-20261009133625-a4bedf5f6000
	github.com/evanw/esbuild v0.28.2
	github.com/go-chi/chi/v5 v5.3.1
	github.com/golang-jwt/jwt/v5 v5.3.1
	github.com/google/uuid v1.6.0
	github.com/jackc/pgx/v5 v5.9.2
	github.com/ledongthuc/pdf v0.0.0-20250511090121-5959a4027728 // UNMAINTAINED (single-owner, stale, no security contact); parses attacker input. KEPT deliberately: best pure-Go text extractor + CGO-free is required. Risk controlled by the killable-subprocess sandbox (internal/distill/isolator.go) + this pin + the govulncheck CI gate. See internal/distill/pdf.go.
	github.com/nats-io/nats-server/v2 v2.14.1
	github.com/nats-io/nats.go v1.52.0
	github.com/pashagolub/pgxmock/v4 v4.9.0
	github.com/prometheus/client_golang v1.23.2
	github.com/prometheus/client_model v0.6.2
	github.com/redis/go-redis/v9 v9.19.0
	github.com/stripe/stripe-go/v81 v81.4.0
	go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp v0.69.0
	go.opentelemetry.io/otel v1.45.0
	go.opentelemetry.io/otel/sdk v1.45.0
	go.opentelemetry.io/otel/trace v1.45.0
	golang.org/x/crypto v0.52.0
	golang.org/x/net v0.55.0
	golang.org/x/sys v0.47.0
	golang.org/x/text v0.41.0
)

require (
	github.com/antithesishq/antithesis-sdk-go v0.7.0-default-no-op // indirect
	github.com/beorn7/perks v1.0.1 // indirect
	github.com/cespare/xxhash/v2 v2.3.0 // indirect
	github.com/dlclark/regexp2/v2 v2.8.1 // indirect
	github.com/felixge/httpsnoop v1.0.4 // indirect
	github.com/go-logr/logr v1.4.4 // indirect
	github.com/go-logr/stdr v1.2.2 // indirect
	github.com/go-sourcemap/sourcemap v2.1.3+incompatible // indirect
	github.com/google/go-tpm v0.9.8 // indirect
	github.com/google/pprof v0.0.0-20230207041349-798e818bf904 // indirect
	github.com/jackc/pgpassfile v1.0.0 // indirect
	github.com/jackc/pgservicefile v0.0.0-20240606120523-5a60cdf6a761 // indirect
	github.com/jackc/puddle/v2 v2.2.2 // indirect
	github.com/klauspost/compress v1.18.7 // indirect
	github.com/kylelemons/godebug v1.1.0 // indirect
	github.com/minio/highwayhash v1.0.4 // indirect
	github.com/munnerz/goautoneg v0.0.0-20191010083416-a7dc8b61c822 // indirect
	github.com/nats-io/jwt/v2 v2.8.1 // indirect
	github.com/nats-io/nkeys v0.4.15 // indirect
	github.com/nats-io/nuid v1.0.1 // indirect
	github.com/prometheus/common v0.66.1 // indirect
	github.com/prometheus/procfs v0.16.1 // indirect
	github.com/yuin/gopher-lua v1.1.1 // indirect
	go.opentelemetry.io/auto/sdk v1.2.1 // indirect
	go.opentelemetry.io/otel/metric v1.45.0 // indirect
	go.uber.org/atomic v1.11.0 // indirect
	go.yaml.in/yaml/v2 v2.4.2 // indirect
	golang.org/x/sync v0.22.0 // indirect
	golang.org/x/time v0.15.0 // indirect
	google.golang.org/protobuf v1.36.8 // indirect
)
