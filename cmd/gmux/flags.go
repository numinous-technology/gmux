package main

import (
	"strconv"

	"github.com/numinous-technology/gmux/internal/daemon"
)

// flagset is a tiny parser: --key value, --key=value, --flag, and everything
// after -- is the command. It is enough for gmux and adds no dependency.
type flagset struct {
	kv    map[string]string
	set   map[string]bool
	multi map[string][]string
	rest  []string
}

func flags(args []string) *flagset {
	f := &flagset{kv: map[string]string{}, set: map[string]bool{}, multi: map[string][]string{}}
	i := 0
	for i < len(args) {
		a := args[i]
		if a == "--" {
			f.rest = args[i+1:]
			break
		}
		if len(a) > 2 && a[:2] == "--" {
			key := a[2:]
			if eq := indexByte(key, '='); eq >= 0 {
				k, v := key[:eq], key[eq+1:]
				f.kv[k] = v
				f.set[k] = true
				f.multi[k] = append(f.multi[k], v)
				i++
				continue
			}
			// a value follows unless the next token is another flag or --
			if i+1 < len(args) && args[i+1] != "--" && !(len(args[i+1]) > 2 && args[i+1][:2] == "--") {
				f.kv[key] = args[i+1]
				f.multi[key] = append(f.multi[key], args[i+1])
				f.set[key] = true
				i += 2
				continue
			}
			f.set[key] = true
			i++
			continue
		}
		i++
	}
	return f
}

func indexByte(s string, b byte) int {
	for i := 0; i < len(s); i++ {
		if s[i] == b {
			return i
		}
	}
	return -1
}

func (f *flagset) str(key, def string) string {
	if v, ok := f.kv[key]; ok {
		return v
	}
	return def
}

func (f *flagset) bool(key string) bool { return f.set[key] }

func (f *flagset) intv(key string, def int) int {
	if v, ok := f.kv[key]; ok {
		if n, err := strconv.Atoi(v); err == nil {
			return n
		}
	}
	return def
}

func (f *flagset) float(key string, def float64) float64 {
	if v, ok := f.kv[key]; ok {
		if n, err := strconv.ParseFloat(v, 64); err == nil {
			return n
		}
	}
	return def
}

func (f *flagset) multiVals(key string) []string { return f.multi[key] }

// convenience used by run()
func (f *flagset) multiList(key string) []string { return f.multi[key] }

var _ = daemon.SubmitRequest{}
