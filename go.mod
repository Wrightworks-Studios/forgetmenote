module forgetmenote

go 1.27.4

require (
	github.com/aws/aws-sdk-go-v2 v1.47.1 // Redshift Data connector and S3 blob store credentials and signing
	github.com/aws/aws-sdk-go-v2/config v1.33.6 // Redshift Data connector and S3 blob store credential chain
	github.com/aws/aws-sdk-go-v2/credentials v1.20.6 // S3 blob store web identity role (stscreds)
	github.com/aws/aws-sdk-go-v2/service/redshiftdata v1.49.0 // Redshift Data connector
	github.com/aws/aws-sdk-go-v2/service/sts v1.51.1 // S3 blob store web identity exchange
	github.com/coreos/go-oidc/v3 v3.21.0 // OIDC sign-in token verification
	github.com/go-jose/go-jose/v4 v4.1.4 // JOSE fixtures for OIDC tests
	github.com/golang/snappy v1.0.0 // iWork archive preflight before the native parser
	github.com/google/jsonschema-go v0.4.3 // typed action and MCP tool schemas
	github.com/jackc/pgx/v5 v5.11.0 // PostgreSQL data connector and instance control plane (pgxpool)
	github.com/microcosm-cc/bluemonday v1.0.27 // rendered Markdown sanitizer
	github.com/modelcontextprotocol/go-sdk v1.7.0 // MCP server and client
	github.com/reearth/ygo v1.49.5 // Yjs live-editing documents
	github.com/richardlehane/mscfb v1.0.9 // Outlook .msg compound-file reader
	github.com/yuin/goldmark v1.8.6 // Markdown rendering
	golang.org/x/image v0.45.0 // BMP, WebP and TIFF decoding for OCR
	golang.org/x/net v0.62.0 // HTML text extraction for captured pages
	golang.org/x/oauth2 v0.36.0 // OIDC, GitHub and connector OAuth clients
	golang.org/x/sys v0.49.0 // rlimits and Landlock for the local extractor sandbox
	golang.org/x/text v0.43.0 // Outlook and RTF legacy charsets
)

require (
	github.com/aws/aws-sdk-go-v2/feature/ec2/imds v1.20.1 // indirect
	github.com/aws/aws-sdk-go-v2/internal/configsources v1.5.4 // indirect
	github.com/aws/aws-sdk-go-v2/internal/endpoints/v2 v2.8.4 // indirect
	github.com/aws/aws-sdk-go-v2/internal/v4a v1.5.4 // indirect
	github.com/aws/aws-sdk-go-v2/service/internal/accept-encoding v1.13.19 // indirect
	github.com/aws/aws-sdk-go-v2/service/internal/presigned-url v1.14.4 // indirect
	github.com/aws/aws-sdk-go-v2/service/signin v1.10.1 // indirect
	github.com/aws/aws-sdk-go-v2/service/sso v1.38.1 // indirect
	github.com/aws/aws-sdk-go-v2/service/ssooidc v1.43.1 // indirect
	github.com/aws/smithy-go v1.28.1 // indirect
	github.com/aymerick/douceur v0.2.0 // indirect
	github.com/gorilla/css v1.0.1 // indirect
	github.com/jackc/pgpassfile v1.0.0 // indirect
	github.com/jackc/pgservicefile v0.0.0-20240606120523-5a60cdf6a761 // indirect
	github.com/jackc/puddle/v2 v2.2.2 // indirect
	github.com/richardlehane/msoleps v1.0.3 // indirect
	github.com/segmentio/asm v1.1.3 // indirect
	github.com/segmentio/encoding v0.5.4 // indirect
	github.com/yosida95/uritemplate/v3 v3.0.2 // indirect
	golang.org/x/sync v0.24.0 // indirect
	golang.org/x/time v0.15.0 // indirect
)
