// Package main implements a GitHub-compatible git forge (single static binary).
//
// scenario.go ports the kit's seeded scenario (sdk/conformance/wire.ts) so the
// forge knows which login the trial's bearer token names. The scenario is pure
// and deterministic: SCENARIO_SEED plus the label "users" always yields the
// same 12 users, whose first login is the viewer (token owner). No clock, no
// randomness, no network is involved.
package main

const u64Mask = 0xffffffffffffffff

// fnv1a64 is FNV-1a over the string's UTF-16 code units (ASCII here).
func fnv1a64(text string) uint64 {
	hash := uint64(0xcbf29ce484222325)
	for i := 0; i < len(text); i++ {
		hash ^= uint64(text[i])
		hash *= 0x100000001b3
	}
	return hash
}

// mix64 is splitmix64's finalizer.
func mix64(v uint64) uint64 {
	z := v + 0x9e3779b97f4a7c15
	z = (z ^ (z >> 30)) * 0xbf58476d1ce4e5b9
	z = (z ^ (z >> 27)) * 0x94d049bb133111eb
	return z ^ (z >> 31)
}

const scenarioSeed = uint64(0x6a09e667f3bcc909)

func scenarioSeedFor(label string) uint64 {
	return mix64(scenarioSeed ^ fnv1a64(label))
}

// rng is xorshift64* with a 64-bit state, exactly as the kit's Rng.
type rng struct{ state uint64 }

func newRng(seed uint64) *rng {
	if seed == 0 {
		seed = scenarioSeed
	}
	return &rng{state: seed & u64Mask}
}

func (r *rng) nextU64() uint64 {
	x := r.state
	x ^= x >> 12
	x ^= x << 25
	x ^= x >> 27
	r.state = x
	return x * 0x2545f4914f6cdd1d
}

func (r *rng) nextU32() uint32 { return uint32(r.nextU64() >> 32) }

// nextInt returns a uniform integer in [0, max), unbiased by rejection.
func (r *rng) nextInt(max int) int {
	if max <= 0 {
		return 0
	}
	if max == 1 {
		return 0
	}
	const range64 = uint64(4294967296)
	limit := uint32(range64 - (range64 % uint64(max)))
	for {
		v := r.nextU32()
		if v < limit {
			return int(v % uint32(max))
		}
	}
}

func (r *rng) pick(items []string) string { return items[r.nextInt(len(items))] }

var scenarioFirstNames = []string{
	"ada", "bram", "cora", "dmitri", "elena", "felix", "greta", "hugo", "iris", "jonas", "kira", "lars",
	"mira", "nico", "olga", "pavel", "quinn", "rosa", "sven", "tess", "ulla", "vito", "wren", "yara", "zane",
}

var scenarioLastNames = []string{
	"anders", "bauer", "costa", "dahl", "eriksen", "fischer", "garcia", "holm", "iversen", "jansen", "keller",
	"lind", "moreau", "novak", "olsen", "peters", "quist", "rossi", "sato", "turi", "ulrich", "vega", "weber",
	"xu", "yilmaz", "zimmer",
}

type scenarioUser struct {
	ID    int
	Login string
	Name  string
	Email string
}

// generateUsers builds `count` users with unique logins, deterministically.
func generateUsers(r *rng, count int) []scenarioUser {
	used := map[string]bool{}
	users := make([]scenarioUser, 0, count)
	for i := 0; i < count; i++ {
		first := r.pick(scenarioFirstNames)
		last := r.pick(scenarioLastNames)
		base := first + last
		login := base
		suffix := 2
		for used[login] {
			login = base + "-" + itoa(suffix)
			suffix++
		}
		used[login] = true
		users = append(users, scenarioUser{ID: 1000 + i, Login: login, Name: first + " " + last, Email: login + "@example.invalid"})
	}
	return users
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	neg := n < 0
	if neg {
		n = -n
	}
	var b [20]byte
	i := len(b)
	for n > 0 {
		i--
		b[i] = byte('0' + n%10)
		n /= 10
	}
	if neg {
		i--
		b[i] = '-'
	}
	return string(b[i:])
}

// scenarioViewerLogin is the login the trial's bearer token names.
func scenarioViewerLogin() string {
	users := generateUsers(newRng(scenarioSeedFor("users")), 12)
	return users[0].Login
}
