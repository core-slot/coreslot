//go:build unix

package main

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/core-slot/coreslot/internal/store"
)

// status names the head, skipping a noted ticket, and shows its valid yield record in text and in
// JSON; a record of another ticket shows nothing. Mutations: reporting the first ticket as head
// names the noted one; dropping the yield from the document leaves it null.
func TestStatusShowsTheHeadAndItsYield(t *testing.T) {
	dir := budgetDir(t, 512)
	c := openLib(t, dir)
	hold(t, c, "gate", 512)
	noted := queue(t, c, "noted", 128)
	head := queue(t, c, "head", 128)
	sec, err := c.Store().Enter(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	until := store.NowNS() + int64(10*time.Minute)
	if err = sec.PublishNote(noted.Name()); err == nil {
		err = sec.SetYield(store.Yield{Head: ticketIDOf(t, head), StartNS: store.NowNS(), UntilNS: until, LastPassNS: 7})
	}
	sec.Leave()
	if err != nil {
		t.Fatal(err)
	}
	doc := statusDoc(t, dir, 0)
	if doc["head"] != ticketIDOf(t, head) {
		t.Errorf("head %v, want %s (the noted ticket is passed over)", doc["head"], ticketIDOf(t, head))
	}
	y, _ := doc["yield"].(map[string]any)
	if y == nil || y["head"] != ticketIDOf(t, head) || y["until_ns"] != float64(until) || y["last_pass_ns"] != 7.0 {
		t.Errorf("yield %v; want the record", doc["yield"])
	}
	want := "letting short jobs go first until " + time.Unix(0, until).Format("15:04")
	if r := cli(t, baseEnv(dir), "status"); !strings.Contains(r.stdout, want) {
		t.Errorf("status lacks %q:\n%s", want, r.stdout)
	}

	sec, err = c.Store().Enter(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	err = sec.SetYield(store.Yield{Head: ticketIDOf(t, noted), StartNS: 1, UntilNS: until})
	sec.Leave()
	if err != nil {
		t.Fatal(err)
	}
	if d := statusDoc(t, dir, 0); d["yield"] != nil {
		t.Errorf("yield %v for a record that names no head; want null", d["yield"])
	}
}
