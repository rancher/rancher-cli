package constantoperand

const prefix = "# Kubeconfig "

const fixed = "# fixed\n"

func header(v string) string {
	return prefix + v + "\n"
}

func plain() []byte {
	return []byte(fixed)
}
