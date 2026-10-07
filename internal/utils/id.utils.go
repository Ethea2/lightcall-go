package utils

import (
	"bytes"
	"math/rand/v2"
)

func GenerateRoomID() string {
	vals := "ABCDEFGHIJKLMNOPQRSTUVWXYZ1234567890"

	var stringBuffer bytes.Buffer
	for i := range 7 {
		if i == 3 {
			stringBuffer.WriteString("-")
			continue
		}
		randomInt := rand.IntN(len(vals))
		stringBuffer.WriteString(string(vals[randomInt]))
	}

	return stringBuffer.String()
}
