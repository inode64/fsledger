package grouping_test

import (
	"fmt"
	"strconv"
	"testing"
	"time"

	"github.com/inode64/fsledger/internal/event"
	"github.com/inode64/fsledger/internal/grouping"
)

func BenchmarkIndependentPaths(b *testing.B) {
	for _, count := range []int{1000, 10000} {
		b.Run(strconv.Itoa(count), func(b *testing.B) {
			paths := make([]string, count)
			for index := range paths {
				paths[index] = fmt.Sprintf("/tree/file-%08d", index)
			}

			now := time.Now()
			owner := actor(123, 456, 1000)

			b.ResetTimer()

			for b.Loop() {
				manager := grouping.New(policy(), time.Second, time.Minute)

				for index, path := range paths {
					owner.PID = 123 + index%2
					manager.Add(event.Raw{Path: path, Time: now, Actor: owner})
				}
			}
		})
	}
}
