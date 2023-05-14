package controller

import "time"

type Clock interface {
	Now() time.Time
}

type realClock struct{}

// 真实的时钟
func (_ realClock) Now() time.Time {
	return time.Now()
}
