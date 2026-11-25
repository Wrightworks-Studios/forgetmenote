package main

import (
	"flag"
	"fmt"
	"forgetmenote/plugins"
	"io"
	"os"
	"path/filepath"
)

func runPlugin(args []string, out io.Writer) error {
	if len(args) == 0 || args[0] != "package" {
		return fmt.Errorf("use: forgetmenote plugin package --origin URL --client codex|claude --output DIRECTORY [--zip]")
	}
	f := flag.NewFlagSet("plugin package", flag.ContinueOnError)
	origin := f.String("origin", "", "canonical HTTPS instance origin")
	name := f.String("name", "ForgetMeNote", "instance display name")
	client := f.String("client", "codex", "codex or claude")
	output := f.String("output", "plugin-local/forgetmenote", "new directory or ZIP file")
	archive := f.Bool("zip", false, "write a ZIP archive")
	if err := f.Parse(args[1:]); err != nil {
		return err
	}
	if f.NArg() != 0 {
		return fmt.Errorf("unexpected plugin arguments")
	}
	in, err := plugins.New(*origin, *name)
	if err != nil {
		return err
	}
	if _, err = in.Files(*client); err != nil {
		return err
	}
	if err = os.MkdirAll(filepath.Dir(*output), 0700); err != nil {
		return err
	}
	if *archive {
		file, e := os.OpenFile(*output, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
		if e != nil {
			return e
		}
		err = in.WriteZip(file, *client)
		closeErr := file.Close()
		if err != nil {
			return err
		}
		if closeErr != nil {
			return closeErr
		}
	} else if err = in.WriteDirectory(*output, *client); err != nil {
		return err
	}
	_, err = fmt.Fprintln(out, *output)
	return err
}
