package cachex

// DoubleCheckMode defines the double-check optimization strategy
type DoubleCheckMode int

const (
	// DoubleCheckDisabled turns off double-check optimization
	DoubleCheckDisabled DoubleCheckMode = iota

	// DoubleCheckEnabled always performs double-check before upstream fetch
	DoubleCheckEnabled

	// DoubleCheckAuto (default) double-checks only when it can find something:
	// when this Client wrote this layer for a key of the same stripe (a Set,
	// Del or backfill) since the request read it. Otherwise the re-read would
	// find the same miss, so it is skipped.
	DoubleCheckAuto
)

// WithDoubleCheck configures the double-check optimization mode.
//
// Default: DoubleCheckAuto.
//
// Singleflight merges the requests that miss a key at the same moment. A
// request that read the layer (a miss) just before another request's fetch
// wrote it, but claims the key only after that fetch has finished, would
// start a fetch of its own. The double-check re-reads this layer (and the
// not-found cache) after claiming the key and returns what it finds instead.
//
// How much it saves depends on that window: roughly how long a read of this
// layer takes to come back, times how many requests the key gets. Measured
// with a key requested 20 times per ms that expires every 20 ms, a 5 ms
// upstream and a layer that answers in 1 ms: 18 upstream calls with the
// double-check, 30 without; with a layer that answers at once (memory), no
// difference. Its cost is one more read of this layer per fetch.
//
// Modes:
//   - DoubleCheckAuto: re-reads only when this Client wrote the key's stripe
//     since the request read the layer. Hot keys get the saving above; keys
//     fetched with nothing written in between (a long tail of cold keys, keys
//     that do not exist) skip the useless re-read. Writes made by other
//     processes to a shared layer are not seen.
//   - DoubleCheckEnabled: always re-reads, also catching writes by other
//     processes to a shared layer, at one more read per fetch.
//   - DoubleCheckDisabled: never re-reads.
func WithDoubleCheck[T any](mode DoubleCheckMode) ClientOption[T] {
	return func(c *Client[T]) {
		c.doubleCheckMode = mode
	}
}
