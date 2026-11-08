package fixture

// unfiltered asks the server for every version, and for another
// Kubernetes version than the cluster's.
func unfiltered() []string { return []string{"&skipFilter=true", "k8sVersion"} }
