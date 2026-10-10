package backyard

import (
	"encoding/json"
	"os"
	"sync"
	"testing"
)

// sdkVectorFile is what the installed Solana and Kamino SDKs returned for the
// exact inputs the parity tests build; testdata/sdk-vectors.json records the
// SDK versions and calls.
type sdkVectorFile struct {
	Base58LeadingZeros struct{ Keys []string }
	MessageV0          struct{ Messages map[string]string }
	KaminoInitializer  struct {
		Instructions []struct {
			Lane, Program, Data, Obligation string
			Accounts                        []struct {
				Address          string
				Signer, Writable bool
			}
		}
		Offsets map[string]int
	}
}

var loadSDKVectors = sync.OnceValues(func() (sdkVectorFile, error) {
	var vectors sdkVectorFile
	raw, err := os.ReadFile("testdata/sdk-vectors.json")
	if err == nil {
		err = json.Unmarshal(raw, &vectors)
	}
	return vectors, err
})

func sdkVectors(t *testing.T) sdkVectorFile {
	t.Helper()
	vectors, err := loadSDKVectors()
	if err != nil {
		t.Fatal("SDK vectors:", err)
	}
	return vectors
}
