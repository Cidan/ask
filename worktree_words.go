package main

// Hand-picked adjective / verb / noun lists used to name ask's
// worktrees: `ask-<provider>-<adjective>-<verb>-<noun>`. 50 entries
// per list gives 50³ = 125,000 combinations, which is wide enough
// that the 8-retry name generator almost never collides.
//
// Entries are screened three ways:
//   1. Each word reads cleanly on its own (no anatomy, violence, or
//      slang).
//   2. Every adjective×verb pair stays innocuous (mood-only adjectives,
//      whimsical action verbs — no "licking", "thrusting", etc.).
//   3. Every verb×noun pair stays innocuous (no anatomical nouns, no
//      nouns that double as anatomical slang).
// Adding entries? Re-screen against all three checks and keep lists at
// exactly 50 — tests lock the count so accidental growth is loud.
var (
	worktreeAdjectives = []string{
		"brave", "bright", "calm", "cheerful", "clever",
		"cozy", "curious", "dapper", "eager", "friendly",
		"gentle", "glowing", "golden", "happy", "humble",
		"jolly", "jovial", "keen", "kind", "lively",
		"lucky", "merry", "mighty", "nimble", "noble",
		"patient", "peaceful", "peppy", "plucky", "polite",
		"quiet", "radiant", "rapid", "royal", "serene",
		"shiny", "silly", "sleek", "snappy", "sparkly",
		"speedy", "spry", "steady", "sunny", "swift",
		"tidy", "tiny", "upbeat", "vivid", "witty",
	}

	worktreeVerbs = []string{
		"baking", "bouncing", "brewing", "chasing", "crafting",
		"cycling", "dancing", "drifting", "exploring", "floating",
		"flying", "gliding", "hopping", "hugging", "humming",
		"juggling", "jumping", "laughing", "leaping", "napping",
		"painting", "prancing", "racing", "reading", "resting",
		"roaming", "rolling", "running", "sailing", "sharing",
		"singing", "skating", "skipping", "sliding", "smiling",
		"sneaking", "soaring", "sparkling", "spinning", "stargazing",
		"strolling", "strumming", "swaying", "swooping", "tumbling",
		"twirling", "wandering", "waving", "whistling", "zooming",
	}

	worktreeNouns = []string{
		"badger", "beaver", "bunny", "butterfly", "cactus",
		"chipmunk", "cloud", "comet", "cricket", "daisy",
		"dolphin", "eagle", "fern", "firefly", "flamingo",
		"fox", "frog", "glacier", "hedgehog", "heron",
		"iris", "jaguar", "kiwi", "koala", "lantern",
		"lemur", "lily", "lynx", "maple", "meadow",
		"moon", "moose", "nebula", "otter", "panda",
		"peach", "pebble", "penguin", "pinecone", "poppy",
		"quartz", "rabbit", "raccoon", "river", "robin",
		"rocket", "seal", "sparrow", "squirrel", "walrus",
	}
)
