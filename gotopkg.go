package main

import (
	"flag"
	"fmt"
	"github.com/groob/plist"
	"html"
	"io/ioutil"
	"log"
	"net/http"
	"os"
	"os/exec"
	"strings"
	"syscall"
	"time"
)

// autopkg process exit code (RECIPE_FAILED_CODE in autopkg)
const RecipeFailureExitCode = 70

type AutopkgRunReport struct {
	Failures []struct {
		Recipe    string `plist:"recipe"`
		Message   string `plist:"message"`
		Traceback string `plist:"traceback"`
	} `plist:"failures"`
	SummaryResults struct {
		URLDownloaderSummaryResult struct {
			DataRows []struct {
				DownloadPath string `plist:"download_path"`
			} `plist:"data_rows"`
			Header      []string `plist:"header"`
			SummaryText string   `plist:"summary_text"`
		} `plist:"url_downloader_summary_result"`
		MunkiImporterSummaryResult struct {
			DataRows []struct {
				PkgRepoPath string `plist:"pkg_repo_path"`
				Catalogs    string `plist:"catalogs"`
				Version     string `plist:"version"`
				PkginfoPath string `plist:"pkginfo_path"`
				Name        string `plist:"name"`
			} `plist:"data_rows"`
			Header      []string `plist:"header"`
			SummaryText string   `plist:"summary_text"`
		} `plist:"munki_importer_summary_result"`
	} `plist:"summary_results"`
}

type currentStatus int
type statusUpdate int

func statusMonitorAndReporter(upd <-chan statusUpdate, cur <-chan chan currentStatus) {
	var theCurrentStatus int = 0
	for {
		select {
		case updateV := <-upd:
			theCurrentStatus += int(updateV)
		case respChan := <-cur:
			go func(x chan currentStatus, y currentStatus) {
				x <- y
			}(respChan, currentStatus(theCurrentStatus))
		}
	}
}

func readStatus(reqChan chan<- chan currentStatus) currentStatus {
	backChan := make(chan currentStatus)
	reqChan <- backChan
	theActualStatus := <-backChan
	close(backChan)
	return theActualStatus
}

var autopkg string

func main() {
	flagAutopkg := flag.String("autopkg", "autopkg", "path to autopkg")
	flag.Parse()

	path, err := exec.LookPath(*flagAutopkg)
	if err != nil {
		log.Fatalf("error finding autopkg binary: %s", err)
	}

	autopkg = path
	log.Printf("using autopkg: %s\n", autopkg)

	if flag.NArg() < 1 {
		log.Fatal("must specify recipe arguments")
	}

	statusUpdate := make(chan statusUpdate)
	getStatus := make(chan chan currentStatus)

	go statusMonitorAndReporter(statusUpdate, getStatus)

	http.HandleFunc("/bar", func(w http.ResponseWriter, r *http.Request) {

		theActualStatus := int(readStatus(getStatus))

		fmt.Fprintf(w, "Hello, %d, %q", theActualStatus, html.EscapeString(r.URL.Path))
	})

	http.HandleFunc("/baz", func(w http.ResponseWriter, r *http.Request) {
		// statusUpdate <- 1
		fmt.Fprintf(w, "updated, %q", html.EscapeString(r.URL.Path))
	})

	go http.ListenAndServe(":8081", nil)

	continuousRun(flag.Args(), statusUpdate)
}

const (
	delayNextCheck      time.Duration = time.Second * 2
	runRecipeNoLessThan               = time.Second * 15
)

type recipeRunStatus struct {
	name        string
	lastStarted time.Time
	firstRun    bool
}

func continuousRun(recipeNames []string, statusUpd chan<- statusUpdate) {
	var recipes []recipeRunStatus

	for _, recipeName := range recipeNames {
		recipes = append(recipes, recipeRunStatus{recipeName, time.Now(), false})
	}

	var lastSince time.Duration

	for {
		for i, recipe := range recipes {
			lastSince = time.Since(recipe.lastStarted)
			if !recipe.firstRun || lastSince > runRecipeNoLessThan {
				recipes[i].firstRun = true
				recipes[i].lastStarted = time.Now()
				log.Printf("running recipe %s (first run or last run > %s [%s])", recipe.name, runRecipeNoLessThan, lastSince)

				autopkgRunOneRecipeCheckDLFirst(recipe.name, false)

				statusUpd <- 1
			}
		}

		log.Printf("sleeping %s", delayNextCheck)
		time.Sleep(delayNextCheck)
	}
}

func autopkgRunOneRecipeCheckDLFirst(recipe string, forceFull bool) {
	var report AutopkgRunReport

	// run, but only check first
	if !forceFull {
		output, err := autopkgRun([]string{"-v", "-c", recipe}, &report)
		log.Printf("check run: %v", report)
		if err != nil {
			log.Printf("error: %s", err)
			if output != nil {
				log.Printf("%s", string(output[:]))
			}
			return
		}
		if output != nil {
			log.Printf("check output: %s", string(output[:]))
		}
		if len(report.SummaryResults.URLDownloaderSummaryResult.DataRows) < 1 {
			log.Printf("nothing downloaded, skipping full run")
			return
		}
	}

	report = AutopkgRunReport{}
	cmdArgs := []string{"-v", recipe}
	if strings.HasSuffix(recipe, "munki") {
		cmdArgs = append(cmdArgs, "MakeCatalogs.munki")
	}
	output, err := autopkgRun(cmdArgs, &report)
	if err != nil {
		log.Printf("%s", err)
	}
	if output != nil {
		log.Printf("run output: %s", string(output[:]))
	}
	if len(report.SummaryResults.MunkiImporterSummaryResult.DataRows) < 1 {
		log.Printf("no munki items imported")
		return
	}

	log.Printf("finished full autopkg run")
}

func autopkgRun(params []string, report *AutopkgRunReport) (output []byte, err error) {
	tmpReport, err := ioutil.TempFile("", "gotopkg")
	if err != nil {
		return nil, fmt.Errorf("error creating report temp file: %v", err)
	}
	tmpReport.Close() // will be overwritten by autopkg anyway

	cmdArgs := []string{"run", "--report-plist", tmpReport.Name()}
	cmdArgs = append(cmdArgs, params...)

	log.Printf("exec autopkg: %s %s", autopkg, strings.Join(cmdArgs, " "))
	output, err = exec.Command(autopkg, cmdArgs...).CombinedOutput()
	if err != nil {
		if exiterr, ok := err.(*exec.ExitError); ok {
			exitCode := exiterr.Sys().(syscall.WaitStatus).ExitStatus()
			// autopkg exit code 70 (RECIPE_FAILED_CODE) means that an autopkg
			// Processor has suffered an exception. because this could be for
			// rather innocuous reasons (and that autopkg itself has caught
			// the problem) we'll refrain from passing on this error. besides
			// it will be included in the report plist, too
			if exitCode != RecipeFailureExitCode {
				return output, fmt.Errorf("error executing autopkg (non-zero return %d): %v", exitCode, err)
			}
		} else {
			return output, fmt.Errorf("error executing autopkg: %v", err)
		}
	}

	f, err := os.Open(tmpReport.Name())
	if err != nil {
		return output, fmt.Errorf("error opening report temp file: %v", err)
	}

	err = plist.NewDecoder(f).Decode(report)
	if err != nil {
		f.Close()
		return output, fmt.Errorf("error decoding report temp file: %v", err)
	}

	f.Close()

	err = os.Remove(tmpReport.Name())
	if err != nil {
		return output, fmt.Errorf("error removing report temp file: %v", err)
	}

	return output, nil
}
