package clusterrequired

import "errors"

func nearMiss() error {
	return errors.New("pass exactly one of --cluster or --all-clusters")
}

func otherRefusal() error {
	return errors.New("--cluster and --all-clusters are mutually exclusive")
}
