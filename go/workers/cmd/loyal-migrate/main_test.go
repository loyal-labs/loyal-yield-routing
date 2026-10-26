package main

import (
	"testing"
)

// The production ledgers were written by the Rust runners. These rows are
// copied from them; a checksum change here would make every deploy refuse.
func TestChecksumsMatchProductionLedgerRows(t *testing.T) {
	for dir, want := range map[string]map[int64]string{
		"../../../../migrations/yield": {
			8:  "d20151ef6d6076961195da6c6cf3b4e11bb3e2045f729bdf4b118f6c7d3ddc34",
			85: "7cec74a15563b3a9406c5f6b5f42139a3dc7e834ecd480002d1edf18086b76d3",
			86: "07d46b137dafb60d5dce35f32d16a74dcab9fb2ec9c4b1730a9c815f1f8a9c14",
		},
		"../../../../migrations/timescale": {
			8: "52c59d576890c355a2737655a4a461c5768e173816724545d388faca0b8b20ad",
		},
	} {
		files, err := load(dir)
		if err != nil {
			t.Fatal(err)
		}
		byVersion := map[int64]migration{}
		for _, m := range files {
			byVersion[m.Version] = m
		}
		for version, sum := range want {
			m, ok := byVersion[version]
			if !ok {
				t.Fatalf("%s: version %d missing", dir, version)
			}
			if m.Checksum != sum {
				t.Fatalf("%s: version %d checksum %s, ledger has %s", dir, version, m.Checksum, sum)
			}
		}
	}
}

func TestCompareSeparatesPendingFromDrift(t *testing.T) {
	files := []migration{{Version: 1, Checksum: "a"}, {Version: 2, Checksum: "b"}, {Version: 3, Checksum: "c"}}
	p := compare(files, map[int64]string{1: "a", 2: "x", 9: "z"})
	if len(p.Applied) != 1 || len(p.Pending) != 1 || p.Pending[0].Version != 3 || len(p.Drift) != 2 {
		t.Fatalf("unexpected plan %+v", p)
	}
}
