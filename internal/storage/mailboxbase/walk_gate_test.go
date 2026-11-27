package mailboxbase

import (
	"testing"
	"time"

	dto "github.com/prometheus/client_model/go"
)

func walkWaits(t *testing.T) uint64 {
	t.Helper()
	var m dto.Metric
	if err := metricWalkWait.Write(&m); err != nil {
		t.Fatal(err)
	}
	return m.GetHistogram().GetSampleCount()
}

// A walk that finds no room waits and does not fail, and the wait is counted;
// a walk heavier than the whole budget takes all of it and runs alone (#2661).
func TestTheWalkBudgetMakesWalksWaitForRoom(t *testing.T) {
	b := newBudget(100)
	first := b.acquire(60)
	waitsBefore := walkWaits(t)
	got := make(chan string, 2)
	go func() {
		release := b.acquire(60)
		got <- "second"
		release()
	}()
	select {
	case <-got:
		t.Fatal("a walk took room the budget did not have")
	case <-time.After(50 * time.Millisecond):
	}
	first()
	if who := <-got; who != "second" {
		t.Fatal(who)
	}
	if walkWaits(t) != waitsBefore+1 {
		t.Error("the wait was not counted")
	}

	// Heavier than the whole budget: it runs, and alone.
	whole := b.acquire(500)
	go func() {
		release := b.acquire(1)
		got <- "small"
		release()
	}()
	select {
	case <-got:
		t.Fatal("a walk ran beside one that holds the whole budget")
	case <-time.After(50 * time.Millisecond):
	}
	whole()
	<-got
}
