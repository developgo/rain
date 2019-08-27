package httptracker

import (
	"strconv"
)

type StatusError struct {
	Code int
	Body string
}

func (e *StatusError) Error() string {
	return "http status: " + strconv.Itoa(e.Code)
}
