package logging

import (
	"log"
	"os"
)

var (
	B2       = log.New(os.Stdout, "[b2] ", log.LstdFlags)
	Alby     = log.New(os.Stdout, "[alby] ", log.LstdFlags)
	Internal = log.New(os.Stdout, "[internal] ", log.LstdFlags)
	HTTP     = log.New(os.Stdout, "[http] ", log.LstdFlags)
)
