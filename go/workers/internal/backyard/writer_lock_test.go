package backyard

import (
	"bufio"
	"context"
	"errors"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/engine"
)

// writerLockHelperEnv runs this test binary as a second Backyard process: it
// waits for the writer lock, says so, and exits (releasing it).
const writerLockHelperEnv = "BACKYARD_WRITER_LOCK_HELPER_URL"

func TestWriterLockHelperProcess(t *testing.T) {
	url := os.Getenv(writerLockHelperEnv)
	if url == "" {
		t.Skip("helper process only")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	if _, err := engine.WaitFamily(ctx, url, engine.FamilyBackyard); err != nil {
		os.Stdout.WriteString("error " + err.Error() + "\n")
		os.Exit(1)
	}
	os.Stdout.WriteString("locked\n")
}

// Two Backyard processes: the second blocks on the writer lock until the
// first one's session ends, and a one-shot operator write refuses while a
// worker holds it.
func TestSecondBackyardProcessBlocksOnTheWriterLock(t *testing.T) {
	url := os.Getenv("PHASE3_TEST_DATABASE_URL")
	if url == "" {
		t.Skip("requires isolated PHASE3_TEST_DATABASE_URL")
	}
	config, err := pgxpool.ParseConfig(url)
	if err != nil || !strings.HasPrefix(config.ConnConfig.Host, "/private/tmp/backyard-phase3-pg.") || config.ConnConfig.Database != "phase3_budget_test" {
		t.Fatal("refusing non-disposable database")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	firstCtx, stopFirst := context.WithCancel(ctx)
	defer stopFirst()
	firstLost, err := engine.WaitFamily(firstCtx, url, engine.FamilyBackyard)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := holdWriterLock(ctx, url); !errors.Is(err, engine.ErrFamilyHeld) {
		t.Fatalf("an operator write took the lock beside the worker: %v", err)
	}

	second := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestWriterLockHelperProcess$", "-test.count=1")
	second.Env = append(os.Environ(), writerLockHelperEnv+"="+url)
	stdout, err := second.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err = second.Start(); err != nil {
		t.Fatal(err)
	}
	lines := make(chan string, 8)
	go func() {
		scanner := bufio.NewScanner(stdout)
		for scanner.Scan() {
			lines <- scanner.Text()
		}
		close(lines)
	}()
	select {
	case line := <-lines:
		t.Fatalf("the second process did not block on the writer lock: %q", line)
	case <-time.After(time.Second):
	}
	stopFirst()
	<-firstLost
	deadline := time.After(15 * time.Second)
	for {
		select {
		case line, ok := <-lines:
			if !ok {
				t.Fatal("the second process exited without the lock")
			}
			if line == "locked" {
				if err := second.Wait(); err != nil {
					t.Fatal(err)
				}
				return
			}
			if strings.HasPrefix(line, "error") {
				t.Fatal(line)
			}
		case <-deadline:
			t.Fatal("the second process never took the released lock")
		}
	}
}
