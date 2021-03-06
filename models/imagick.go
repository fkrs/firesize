package models

import (
	"bytes"
	"fmt"
	"io"
	"io/ioutil"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/technoweenie/grohl"
)

// Low timeout as we can potentially run 3 convert steps and in all
// cases in a Heroku environment there is a hard limit of 30secs before
// your request is simply killed
var normalTimeout = 10 * time.Second

type IMagick struct{}

type processPipelineStep func(workingDirectoryPath string, inputFilePath string, args *ProcessArgs) (outputFilePath string, err error)

var defaultPipeline = []processPipelineStep{
	downloadRemote,
	preProcessImage,
	processImage,
	postProcessImage,
}

// Process a remote asset url using graphicsmagick with the args supplied
// and write the response to w
func (p *IMagick) Process(w http.ResponseWriter, r *http.Request, args *ProcessArgs) (err error) {
	tempDir, err := createTemporaryWorkspace()
	if err != nil {
		return
	}
	// defer os.RemoveAll(tempDir)

	var filePath string

	// No operations? Just proxy the request
	if !args.HasOperations() {
		return proxyRequest(w, args)
	}

	for _, step := range defaultPipeline {
		filePath, err = step(tempDir, filePath, args)
		if err != nil {
			return
		}
	}

	// serve response
	http.ServeFile(w, r, filePath)
	return
}

func createTemporaryWorkspace() (string, error) {
	return ioutil.TempDir("", "_firesize")
}

func proxyRequest(w http.ResponseWriter, args *ProcessArgs) error {
	resp, err := http.Get(args.Url)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	_, err = io.Copy(w, resp.Body)
	return err
}

func downloadRemote(tempDir string, _ string, args *ProcessArgs) (string, error) {
	url := args.Url
	inFile := filepath.Join(tempDir, "in")

	grohl.Log(grohl.Data{
		"processor": "imagick",
		"download":  url,
		"local":     inFile,
	})

	out, err := os.Create(inFile)
	if err != nil {
		return inFile, err
	}
	defer out.Close()

	resp, err := http.Get(url)
	if err != nil {
		return inFile, err
	}
	defer resp.Body.Close()

	_, err = io.Copy(out, resp.Body)

	return inFile, err
}

func preProcessImage(tempDir string, inFile string, args *ProcessArgs) (string, error) {
	if isAnimatedGif(inFile) {
		args.Format = "gif" // Total hack cos format is incorrectly .png on example
		return coalesceAnimatedGif(tempDir, inFile)
	} else {
		return inFile, nil
	}
}

func processImage(tempDir string, inFile string, args *ProcessArgs) (string, error) {
	outFile := filepath.Join(tempDir, "out")
	cmdArgs, outFileWithFormat := args.CommandArgs(inFile, outFile)

	grohl.Log(grohl.Data{
		"processor": "imagick",
		"args":      cmdArgs,
	})

	executable := "convert"
	cmd := exec.Command(executable, cmdArgs...)
	var outErr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &outErr, &outErr
	err := runWithTimeout(cmd, normalTimeout)
	if err != nil {
		grohl.Log(grohl.Data{
			"processor": "imagick",
			"step":      "convert",
			"failure":   err,
			"args":      cmdArgs,
			"output":    string(outErr.Bytes()),
		})
	}

	return outFileWithFormat, err
}

func postProcessImage(tempDir string, inFile string, args *ProcessArgs) (string, error) {
	// If it originally "mp4" was requested even if before processing
	// changed it to "gif"
	grohl.Log(grohl.Data{"args": args})
	if args.RequestFormat == "mp4" && args.Format == "gif" {
		outFile := filepath.Join(tempDir, "video.mp4")
		cmdArgs := []string{"-f", "gif", "-i", inFile, outFile}

		grohl.Log(grohl.Data{
			"processor": "ffmpeg",
			"args":      cmdArgs,
		})

		cmd := exec.Command("ffmpeg", cmdArgs...)
		var outErr bytes.Buffer
		cmd.Stdout, cmd.Stderr = &outErr, &outErr
		err := runWithTimeout(cmd, normalTimeout)
		if err != nil {
			grohl.Log(grohl.Data{
				"processor": "ffmpeg",
				"step":      "post-process-mp4",
				"failure":   err,
				"args":      cmdArgs,
				"output":    string(outErr.Bytes()),
			})
		}

		return outFile, err
	}

	return inFile, nil
}

func isAnimatedGif(inFile string) bool {
	// identify -format %n updates-product-click.gif # => 105
	cmd := exec.Command("identify", "-format", "%n", inFile)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	err := runWithTimeout(cmd, 10*time.Second)
	if err != nil {
		output := string(stderr.Bytes())
		grohl.Log(grohl.Data{
			"processor": "imagick",
			"step":      "identify",
			"failure":   err,
			"output":    output,
		})
	} else {
		output := string(stdout.Bytes())
		output = strings.TrimSpace(output)
		numFrames, err := strconv.Atoi(output)
		if err != nil {
			grohl.Log(grohl.Data{
				"processor": "imagick",
				"step":      "identify",
				"failure":   err,
				"output":    output,
				"message":   "non numeric identify output",
			})
		} else {
			grohl.Log(grohl.Data{
				"processor":  "imagick",
				"step":       "identify",
				"num-frames": numFrames,
			})
			return numFrames > 1
		}
	}
	// if anything fucks out assume not animated
	return false
}

func coalesceAnimatedGif(tempDir string, inFile string) (string, error) {
	outFile := filepath.Join(tempDir, "temp")

	// convert do.gif -coalesce temporary.gif
	cmd := exec.Command("convert", inFile, "-coalesce", outFile)
	var outErr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &outErr, &outErr

	err := runWithTimeout(cmd, 60*time.Second)
	if err != nil {
		grohl.Log(grohl.Data{
			"processor": "imagick",
			"step":      "coalesce",
			"failure":   err,
			"output":    string(outErr.Bytes()),
		})
	}

	return outFile, err
}

func runWithTimeout(cmd *exec.Cmd, timeout time.Duration) error {
	// Start the process
	err := cmd.Start()
	if err != nil {
		return err
	}

	// Kill the process if it doesn't exit in time
	defer time.AfterFunc(timeout, func() {
		fmt.Println("command timed out")
		cmd.Process.Kill()
	}).Stop()

	// Wait for the process to finish
	return cmd.Wait()
}
