package config

import "embed"

//go:embed cns.vmware.com_storageclassproperties.yaml
var EmbedStorageClassPropertiesCRFile embed.FS

const EmbedStorageClassPropertiesCRFileName = "cns.vmware.com_storageclassproperties.yaml"
