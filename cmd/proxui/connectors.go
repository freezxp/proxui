package main

// This file is the connector manifest: the only place in the tree that names
// concrete connectors. Each blank import runs the package's init(), which
// registers its factory with internal/connector's registry. Adding a platform
// means adding a package and one line here (docs/09-connector-architecture.md).
import (
	// The mock platform ships in every build: it backs the dev environment,
	// the test suite and the load-test fixture, and it is what proves the core
	// works without any real hypervisor.
	_ "github.com/freezxp/proxui/internal/connectors/mock"
	_ "github.com/freezxp/proxui/internal/connectors/proxmox"
)
