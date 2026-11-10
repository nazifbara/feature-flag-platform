module github.com/nazifbara/flagsvc/api

go 1.27.2

require (
	connectrpc.com/connect/v2 v2.0.1
	connectrpc.com/validate v0.9.0
	github.com/alexedwards/argon2id v1.0.0
	github.com/google/go-cmp v0.7.0
	github.com/lib/pq v1.12.3
	github.com/nazifbara/flagsvc/evaluator v0.0.0
	github.com/nazifbara/flagsvc/gen/go v0.0.0
	google.golang.org/protobuf v1.36.12
)

require (
	buf.build/gen/go/bufbuild/protovalidate/protocolbuffers/go v1.36.12-20260825204119-511051f7f437.2 // indirect
	buf.build/go/protovalidate v1.4.0 // indirect
	cel.dev/cel-go v0.32.0 // indirect
	cel.dev/expr v0.25.3 // indirect
	connectrpc.com/connect v1.21.1 // indirect
	github.com/antlr4-go/antlr/v4 v4.13.1 // indirect
	go.yaml.in/yaml/v3 v3.0.5 // indirect
	golang.org/x/crypto v0.14.0 // indirect
	golang.org/x/exp v0.0.0-20261014143641-f0251eb4aa3e // indirect
	golang.org/x/sys v0.47.0 // indirect
	golang.org/x/text v0.43.0 // indirect
	google.golang.org/genproto/googleapis/api v0.0.0-20261012220534-4341edf3f941 // indirect
	google.golang.org/genproto/googleapis/rpc v0.0.0-20261012220534-4341edf3f941 // indirect
)

replace github.com/nazifbara/flagsvc/evaluator => ../evaluator

replace github.com/nazifbara/flagsvc/gen/go => ../gen/go
