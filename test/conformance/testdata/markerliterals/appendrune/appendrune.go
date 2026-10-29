package appendrune

import "unicode/utf8"

func command() string { return string(utf8.AppendRune(nil, 0x2063)) }
