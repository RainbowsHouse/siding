module github.com/RainbowsHouse/siding

// Traefik interprets plugins with Yaegi v0.16.1 (every release from v3.3 to
// v3.7), whose stdlib symbols stop at Go 1.22: keep to that language level (no
// range-over-int/func, no min/max builtins, no slices/maps packages).
go 1.22
