package runner

import (
	"io/fs"
	"os"
	"os/exec"
	"strings"
)

// lookPath finds the command as the shell that will not be involved would
// have: by the PATH the command itself is given.
func lookPath(name string, env []string, dir string) (string, error) {
	if strings.ContainsRune(name, os.PathSeparator) {
		st, err := os.Stat(resolveIn(dir, name))
		if err != nil {
			return "", err
		}
		if st.IsDir() || st.Mode()&0o111 == 0 {
			return "", fs.ErrPermission
		}
		return name, nil
	}
	for _, folder := range strings.Split(getEnv(env, "PATH"), string(os.PathListSeparator)) {
		if folder == "" {
			folder = "."
		}
		candidate := folder + string(os.PathSeparator) + name
		if st, err := os.Stat(resolveIn(dir, candidate)); err == nil && !st.IsDir() && st.Mode()&0o111 != 0 {
			return candidate, nil
		}
	}
	return "", exec.ErrNotFound
}

func resolveIn(dir, path string) string {
	if strings.HasPrefix(path, string(os.PathSeparator)) {
		return path
	}
	return dir + string(os.PathSeparator) + path
}

func getEnv(env []string, key string) string {
	// The last one wins, as it does for the process that gets them.
	for i := len(env) - 1; i >= 0; i-- {
		if v, ok := strings.CutPrefix(env[i], key+"="); ok {
			return v
		}
	}
	return ""
}

func setEnv(env []string, key, value string) []string {
	out := make([]string, 0, len(env)+1)
	for _, kv := range env {
		if !strings.HasPrefix(kv, key+"=") {
			out = append(out, kv)
		}
	}
	return append(out, key+"="+value)
}
