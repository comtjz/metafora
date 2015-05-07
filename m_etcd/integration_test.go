package m_etcd_test

import (
	"errors"
	"testing"
	"time"

	"github.com/coreos/go-etcd/etcd"
	"github.com/lytics/metafora"
	"github.com/lytics/metafora/m_etcd"
	"github.com/lytics/metafora/m_etcd/testutil"
	"github.com/lytics/metafora/statemachine"
)

// TestNew is an integration test for m_etcd's New function.
func TestNew(t *testing.T) {
	t.Parallel()
	etcdc := testutil.NewEtcdClient(t)

	const recursive = true
	etcdc.Delete("test-a", recursive)
	etcdc.Delete("test-b", recursive)

	h := func(tid string, cmds <-chan statemachine.Message) statemachine.Message {
		cmd := <-cmds
		if tid == "error-test" {
			return statemachine.Message{Code: statemachine.Error, Err: errors.New("error-test")}
		}
		return cmd
	}

	newC := func(name, ns string) *metafora.Consumer {
		coord, hf, bal := m_etcd.New(name, ns, etcdc, h)
		cons, err := metafora.NewConsumer(coord, hf, bal)
		if err != nil {
			t.Fatalf("Error creating consumer %s:%s: %v", ns, name, err)
		}
		go cons.Run()
		return cons
	}
	// Start 4 consumers, 2 per namespace
	cons1a := newC("node1", "test-a")
	cons2a := newC("node2", "test-a")
	cons1b := newC("node1", "test-b")
	cons2b := newC("node2", "test-b")

	// Create clients and start some tests
	cliA := m_etcd.NewClient("test-a", etcdc)
	cliB := m_etcd.NewClient("test-b", etcdc)

	if err := cliA.SubmitTask("task1"); err != nil {
		t.Fatalf("Error submitting task1 to a: %v", err)
	}
	if err := cliB.SubmitTask("task1"); err != nil {
		t.Fatalf("Error submitting task1 to b: %v", err)
	}

	// Give consumers a bit to pick up tasks
	time.Sleep(250 * time.Millisecond)

	assertRunning := func(tid string, cons ...*metafora.Consumer) {
		found := false
		for _, c := range cons {
			tasks := c.Tasks()
			if len(tasks) > 0 && found {
				t.Fatal("Task already found running but another task is running on a different consumer")
			}
			if len(tasks) > 1 {
				t.Fatalf("Expected at most 1 task, but found: %d", len(tasks))
			}
			if len(tasks) == 1 && tasks[0].ID() == tid {
				found = true
			}
		}
		if !found {
			t.Fatalf("Could not find task=%q", tid)
		}
	}

	assertRunning("task1", cons1a, cons2a)
	assertRunning("task1", cons1b, cons2b)

	// Kill task1 in A
	{
		cmdr := m_etcd.NewCommander("test-a", etcdc)
		if err := cmdr.Send("task1", statemachine.Message{Code: statemachine.Kill}); err != nil {
			t.Fatalf("Error sending kill to task1: %v", err)
		}
		time.Sleep(250 * time.Millisecond)

		for _, c := range []*metafora.Consumer{cons1a, cons2a} {
			tasks := c.Tasks()
			if len(tasks) != 0 {
				t.Fatalf("Expected no tasks but found: %d", len(tasks))
			}
		}
	}

	// Submit a bunch of tasks to A
	{
		tasks := map[string]int{"task2": 1, "task3": 1, "task4": 1, "task5": 1, "task6": 1, "task7": 1}
		for tid := range tasks {
			if err := cliA.SubmitTask(tid); err != nil {
				t.Fatalf("Error submitting task=%q to a: %v", tid, err)
			}
		}

		// Give them time to start
		time.Sleep(500 * time.Millisecond)

		// Ensure they're balanced
		if err := cliA.SubmitCommand("node1", metafora.CommandBalance()); err != nil {
			t.Fatalf("Error submitting balance command to cons1a: %v", err)
		}
		time.Sleep(500 * time.Millisecond)
		if err := cliA.SubmitCommand("node2", metafora.CommandBalance()); err != nil {
			t.Fatalf("Error submitting balance command to cons1a: %v", err)
		}

		a1tasks := cons1a.Tasks()
		a2tasks := cons2a.Tasks()
		for _, task := range a1tasks {
			metafora.Debug("A1: ", task.ID(), " - ", task.Stopped().IsZero())
		}
		for _, task := range a2tasks {
			metafora.Debug("A2: ", task.ID(), " - ", task.Stopped().IsZero())
		}
		time.Sleep(500 * time.Millisecond)

		a1tasks = cons1a.Tasks()
		a2tasks = cons2a.Tasks()
		if len(a1tasks) < 2 || len(a1tasks) > 4 || len(a2tasks) < 2 || len(a2tasks) > 4 {
			t.Fatalf("Namespace A isn't fairly balanced: node1: %d; node2: %d", len(a1tasks), len(a2tasks))
		}

		// Shutting down a consumer should migrate all tasks to the other
		cons1a.Shutdown()
		time.Sleep(500 * time.Millisecond)

		a2tasks = cons2a.Tasks()
		if len(a2tasks) != len(tasks) {
			t.Fatalf("Consumer 2a should have received all %d tasks but only has %d.", len(tasks), len(a2tasks))
		}
	}

	// Shutdown
	cons2a.Shutdown()
	cons1b.Shutdown()
	cons2b.Shutdown()

	// Make sure everything is cleaned up
	respA, err := etcdc.Get("/test-a/tasks", true, true)
	if err != nil {
		t.Fatalf("Error getting tasks from etcd: %v", err)
	}
	respB, err := etcdc.Get("/test-b/tasks", true, true)
	if err != nil {
		t.Fatalf("Error getting tasks from etcd: %v", err)
	}

	nodes := []*etcd.Node{}
	nodes = append(nodes, respA.Node.Nodes...)
	nodes = append(nodes, respB.Node.Nodes...)
	for _, node := range nodes {
		if len(node.Nodes) > 0 {
			t.Fatalf("%s has %d nodes. First key: %s", node.Key, len(node.Nodes), node.Nodes[0].Key)
		}
	}
}
