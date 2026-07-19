# Connector v1 external build fixture

This is a dependency-free contract/build proof for XOD-85. It demonstrates
that a standalone Rust program can bind an open string `type_id` and implement
and invoke all eight RPC names from the frozen Connector v1 surface without
importing an OLP implementation crate or coupling to the closed core provider
enum.

It deliberately does **not** claim to be a gRPC connector, generated protobuf
binding, published SDK, runtime integration, conformance suite, or qualified
reference connector. Those remain M4/M8 work. The machine contract pins both
the fixture source and reviewed protobuf digests so a source or wire change
forces this proof to be reviewed again.

Run `scripts/check-reference-connector-v1.sh` to compile and inspect it.
