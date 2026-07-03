package config

// legacyConfig is the minimal subset of cli2.json needed for import.
type legacyConfig struct {
	CurrentServer string                   `json:"CurrentServer"`
	Servers       map[string]*legacyServer `json:"Servers"`
}

type legacyServer struct {
	AccessKey string `json:"accessKey"`
	SecretKey string `json:"secretKey"`
	TokenKey  string `json:"tokenKey"`
	URL       string `json:"url"`
	Project   string `json:"project"` // "<cluster>:<project>"
	CACerts   string `json:"cacert"`
}
