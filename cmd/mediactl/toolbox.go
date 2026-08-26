package main

import (
	"crypto/sha256"
	"encoding/binary"
	"flag"
	"fmt"
	"io"
	"math/rand"
	"os"
	"strings"

	"github.com/Cloudbird-Software/Media-Monitor/internal/toolbox/encrypt"
	"github.com/Cloudbird-Software/Media-Monitor/internal/toolbox/wechat"
)

// toolbox.go — the local content tools: zero-width text steganography
// (encrypt embed/extract), phone-number stylization (stylize), and the
// WeChat multi-open helper (wechat-multi). All transformations are local
// (no platform endpoint); the toolbox is exempt from the license gate.

func cmdToolbox(args []string) error {
	if len(args) == 0 {
		return fmt.Errorf("use: toolbox encrypt|stylize|wechat-multi (see mediactl help)")
	}
	switch args[0] {
	case "encrypt":
		return toolboxEncrypt(args[1:])
	case "stylize":
		return toolboxStylize(args[1:])
	case "wechat-multi":
		return toolboxWechatMulti(args[1:])
	default:
		return fmt.Errorf("unknown toolbox subcommand %q", args[0])
	}
}

func toolboxEncrypt(args []string) error {
	if len(args) == 0 {
		return fmt.Errorf("use: toolbox encrypt embed|extract")
	}
	switch args[0] {
	case "embed":
		return toolboxEmbed(args[1:])
	case "extract":
		return toolboxExtract(args[1:])
	default:
		return fmt.Errorf("unknown toolbox encrypt subcommand %q", args[0])
	}
}

// readTextArg returns the --text value or, when empty, stdin (trailing line
// terminators trimmed).
func readTextArg(text string) (string, error) {
	if text != "" {
		return text, nil
	}
	b, err := io.ReadAll(os.Stdin)
	if err != nil {
		return "", fmt.Errorf("read stdin: %w", err)
	}
	return strings.TrimRight(string(b), "\r\n"), nil
}

func toolboxEmbed(args []string) error {
	fs := flag.NewFlagSet("toolbox encrypt embed", flag.ExitOnError)
	text := fs.String("text", "", "text to hide (empty = read stdin)")
	secret := fs.String("secret", "", "optional secret seeding the zero-width pattern deterministically (same secret = same pattern)")
	min := fs.Int("min", encrypt.DefaultRandomMin, "min zero-width run length")
	max := fs.Int("max", encrypt.DefaultRandomMax, "max zero-width run length")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *min < 0 || *max < *min {
		return fmt.Errorf("require 0 <= --min <= --max, got %d..%d", *min, *max)
	}
	in, err := readTextArg(*text)
	if err != nil {
		return err
	}
	var rng *rand.Rand
	if *secret != "" {
		sum := sha256.Sum256([]byte(*secret))
		rng = rand.New(rand.NewSource(int64(binary.LittleEndian.Uint64(sum[:8]))))
	}
	fmt.Println(encrypt.Embed(in, *min, *max, rng))
	return nil
}

func toolboxExtract(args []string) error {
	fs := flag.NewFlagSet("toolbox encrypt extract", flag.ExitOnError)
	text := fs.String("text", "", "text to restore (empty = read stdin)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	in, err := readTextArg(*text)
	if err != nil {
		return err
	}
	fmt.Println(encrypt.Extract(in))
	return nil
}

func toolboxStylize(args []string) error {
	fs := flag.NewFlagSet("toolbox stylize", flag.ExitOnError)
	phone := fs.String("phone", "", "single phone number")
	file := fs.String("phones-file", "", "file with one phone number per line")
	style := fs.Bool("style", false, "固定风格: apply one fixed unified style per number")
	sep := fs.Bool("separator", false, "insert a random separator between consecutive digits")
	if err := fs.Parse(args); err != nil {
		return err
	}
	var input string
	switch {
	case *phone != "" && *file != "":
		return fmt.Errorf("use only one of --phone / --phones-file")
	case *phone != "":
		input = *phone
	case *file != "":
		b, err := os.ReadFile(*file)
		if err != nil {
			return fmt.Errorf("phones-file: %w", err)
		}
		input = string(b)
	default:
		b, err := io.ReadAll(os.Stdin)
		if err != nil {
			return fmt.Errorf("read stdin: %w", err)
		}
		input = string(b)
	}
	input = strings.TrimRight(input, "\r\n")
	if strings.TrimSpace(input) == "" {
		return fmt.Errorf("no phone numbers given (--phone, --phones-file or stdin)")
	}
	fmt.Println(encrypt.StylizeBatch(input, *sep, *style, nil))
	return nil
}

// wechatLauncher is the process launcher handed to wechat.Launch; nil means
// the real exec. Tests inject a fake.
var wechatLauncher wechat.Launcher

func toolboxWechatMulti(args []string) error {
	fs := flag.NewFlagSet("toolbox wechat-multi", flag.ExitOnError)
	num := fs.Int("num", 0, "number of WeChat instances to launch (>= 1)")
	helper := fs.String("helper-path", "", "openwechat.exe path (default: extraResources/more/openwechat.exe next to the executable)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *num < 1 {
		return fmt.Errorf("--num must be >= 1, got %d", *num)
	}
	if err := wechat.Launch(wechat.Config{HelperPath: *helper, Launcher: wechatLauncher}, *num); err != nil {
		return err
	}
	fmt.Printf("launched %d wechat instance(s)\n", *num)
	return nil
}
