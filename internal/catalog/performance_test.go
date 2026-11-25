package catalog

import (
	"fmt"
	"forgetmenote/internal/pg/pgtest"
	"strings"
	"testing"
)

func BenchmarkCatalogUpdate(b *testing.B) {
	for _, count := range []int{100, 1000} {
		b.Run(fmt.Sprint(count), func(b *testing.B) {
			projects, err := Open(b.TempDir(), nil, pgtest.Org(b))
			if err != nil {
				b.Fatal(err)
			}
			defer projects.Close()
			for i := 0; i < count; i++ {
				if err := projects.SetUser(Bootstrap, User{ID: fmt.Sprintf("user%d", i)}); err != nil {
					b.Fatal(err)
				}
			}
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				if err := projects.SetUser(Bootstrap, User{ID: "user0", DisplayName: fmt.Sprintf("Revision %d", i)}); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}

func BenchmarkCatalogActivityUpdate(b *testing.B) {
	c, err := Open(b.TempDir(), nil, pgtest.Org(b))
	if err != nil {
		b.Fatal(err)
	}
	defer c.Close()
	if err = c.SetUser(Bootstrap, User{ID: "bench"}); err != nil {
		b.Fatal(err)
	}
	for i := 0; i < activityLimit; i++ {
		c.state.ActivitySequence++
		c.activity = append(c.activity, Activity{ID: c.state.ActivitySequence, Actor: "bench", Action: "fixture", Target: "default", Outcome: "succeeded", Details: map[string]string{"context": strings.Repeat("bounded evidence ", 12)}})
	}
	c.activity = c.activity[len(c.activity)-activityLimit:]
	if err = c.persist(); err != nil {
		b.Fatal(err)
	}
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if err := c.RecordActivity(Bootstrap, "measured", "default", "succeeded", nil); err != nil {
			b.Fatal(err)
		}
	}
}
