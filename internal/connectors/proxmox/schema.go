package proxmox

import "github.com/freezxp/proxui/internal/connector"

// schema declares the platform form for Proxmox.
//
// The help text carries the two things that actually cost an administrator
// time here: the API token has to be created in the cluster first and cannot
// be read back afterwards, and a default Proxmox install presents a
// self-signed certificate, which is why a fingerprint pin is offered rather
// than only "verify or don't".
func schema() connector.ConfigSchema {
	return connector.ConfigSchema{
		EndpointLabel: "API URL",
		EndpointHelp:  "The cluster API address, including the port — for example https://pve.example.com:8006",
		Fields: []connector.Field{
			{
				Key: "node_filter", Label: "Node filter", Kind: connector.FieldText,
				Placeholder: "pve1,pve2",
				Help:        "Optional. Only synchronize these nodes, comma separated. Leave empty for the whole cluster.",
			},
			{
				Key: "include_templates", Label: "Include templates", Kind: connector.FieldBool,
				Default: false,
				Help:    "Synchronize VM templates alongside running guests.",
			},
		},
		Credentials: []connector.CredentialForm{
			{
				Kind:  "api_token",
				Label: "API token",
				Help:  "Create the token in Datacenter → Permissions → API Tokens. The secret is shown once, at creation.",
				Fields: []connector.Field{
					{
						Key: "token_id", Label: "Token ID", Kind: connector.FieldText, Required: true,
						Placeholder: "proxui@pve!portal",
						Help:        "The full identifier, in the form user@realm!tokenname.",
					},
					{
						Key: "secret", Label: "Token secret", Kind: connector.FieldSecret, Required: true,
						Help: "Stored encrypted and never shown again.",
					},
				},
			},
			{
				Kind:  "userpass",
				Label: "Username and password",
				Help:  "Works without creating a token, but the portal then holds a login rather than a scoped credential. Prefer a token.",
				Fields: []connector.Field{
					{
						Key: "username", Label: "Username", Kind: connector.FieldText, Required: true,
						Placeholder: "proxui@pve",
					},
					{Key: "password", Label: "Password", Kind: connector.FieldSecret, Required: true},
				},
			},
		},
	}
}
