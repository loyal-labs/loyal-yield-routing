package chain

import (
	"context"
	"time"
)

// BlockTime is slot's estimated production time. ErrNotFound means the
// cluster keeps no time for that block.
func (c *Client) BlockTime(ctx context.Context, slot uint64) (time.Time, error) {
	out, err := c.rpc.GetBlockTime(ctx, slot)
	if err != nil {
		return time.Time{}, failed("getBlockTime", err)
	}
	if out == nil {
		return time.Time{}, ErrNotFound
	}
	return out.Time().UTC(), nil
}
