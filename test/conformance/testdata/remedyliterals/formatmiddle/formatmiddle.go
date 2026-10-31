package formatmiddle

import "fmt"

func command(name string) error {
	_ = fmt.Sprintf("check with rancher %s", name)
	return fmt.Errorf("rancher %s failed", name)
}
