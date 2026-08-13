package main

// This file is the connector manifest: the only place in the tree that names
// concrete connectors. Each blank import runs the package's init(), which
// registers its factory with internal/connector's registry. Adding a platform
// means adding a package and one line here (docs/09-connector-architecture.md).
//
// Connectors land in sprints 4 (mock) and 5 (proxmox).
import (
// _ "github.com/freezxp/proxui/internal/connectors/mock"
// _ "github.com/freezxp/proxui/internal/connectors/proxmox"
)
