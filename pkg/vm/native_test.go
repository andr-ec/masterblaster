package vm

import "testing"

func TestVanishedSourceOnly(t *testing.T) {
	cases := []struct {
		name   string
		stderr string
		want   bool
	}{
		{
			name:   "empty stderr is not a tolerable failure on its own",
			stderr: "",
			want:   false,
		},
		{
			name:   "single vanished file (cannot stat ENOENT)",
			stderr: "cp: cannot stat '/home/u/p/.claude/solutions': No such file or directory",
			want:   true,
		},
		{
			name: "multiple vanished files, mixed read-failure phrasings",
			stderr: "cp: cannot stat '/p/.claude/audit': No such file or directory\n" +
				"cp: cannot open '/p/.claude/x' for reading: No such file or directory",
			want: true,
		},
		{
			name:   "non-reflink filesystem stays fatal",
			stderr: "cp: failed to clone '/p/big.bin' from '/src/big.bin': Operation not supported",
			want:   false,
		},
		{
			name:   "permission denied stays fatal",
			stderr: "cp: cannot open '/p/secret' for reading: Permission denied",
			want:   false,
		},
		{
			name:   "out of space stays fatal",
			stderr: "cp: error writing '/dst/x': No space left on device",
			want:   false,
		},
		{
			name: "any fatal line among vanished lines makes the whole batch fatal",
			stderr: "cp: cannot stat '/p/.claude/a': No such file or directory\n" +
				"cp: failed to clone '/p/b': Operation not supported",
			want: false,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := vanishedSourceOnly(tc.stderr); got != tc.want {
				t.Fatalf("vanishedSourceOnly(%q) = %v, want %v", tc.stderr, got, tc.want)
			}
		})
	}
}
