package src

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/ioutil"
	"log"
	"net/url"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"sourcegraph.com/sourcegraph/rwvfs"
	"sourcegraph.com/sourcegraph/s3vfs"
	"sourcegraph.com/sourcegraph/srclib/config"
	"sourcegraph.com/sourcegraph/srclib/graph"
	"sourcegraph.com/sourcegraph/srclib/grapher"
	"sourcegraph.com/sourcegraph/srclib/plan"
	"sourcegraph.com/sourcegraph/srclib/store"
	"sourcegraph.com/sourcegraph/srclib/unit"
)

func init() {
	c, err := CLI.AddCommand("store",
		"graph store commands",
		"",
		&storeCmd,
	)
	if err != nil {
		log.Fatal(err)
	}
	lrepo, _ := openLocalRepo()
	if lrepo != nil && lrepo.RootDir != "" {
		absDir, err := os.Getwd()
		if err != nil {
			log.Fatal(err)
		}
		relDir, err := filepath.Rel(absDir, lrepo.RootDir)
		if err == nil {
			SetOptionDefaultValue(c.Group, "root", filepath.Join(relDir, store.SrclibStoreDir))
		}
	}

	importC, err := c.AddCommand("import",
		"import data",
		`The import command imports data (from .srclib-cache) into the store.`,
		&storeImportCmd,
	)
	if err != nil {
		log.Fatal(err)
	}
	setDefaultRepoURIOpt(importC)
	setDefaultCommitIDOpt(importC)

	_, err = c.AddCommand("indexes",
		"list indexes",
		"The indexes command lists all of a store's indexes that match the specified criteria.",
		&storeIndexesCmd,
	)
	if err != nil {
		log.Fatal(err)
	}

	_, err = c.AddCommand("index",
		"build indexes",
		"The index command builds indexes that match the specified index criteria. Built indexes are printed to stdout.",
		&storeIndexCmd,
	)
	if err != nil {
		log.Fatal(err)
	}

	_, err = c.AddCommand("repos",
		"list repos",
		"The repos command lists all repos that match a filter.",
		&storeReposCmd,
	)
	if err != nil {
		log.Fatal(err)
	}

	_, err = c.AddCommand("versions",
		"list versions",
		"The versions command lists all versions that match a filter.",
		&storeVersionsCmd,
	)
	if err != nil {
		log.Fatal(err)
	}

	_, err = c.AddCommand("units",
		"list units",
		"The units command lists all units that match a filter.",
		&storeUnitsCmd,
	)
	if err != nil {
		log.Fatal(err)
	}

	defsC, err := c.AddCommand("defs",
		"list defs",
		"The defs command lists all defs that match a filter.",
		&storeDefsCmd,
	)
	if err != nil {
		log.Fatal(err)
	}
	defsC.Aliases = []string{"def"}

	_, err = c.AddCommand("refs",
		"list refs",
		"The refs command lists all refs that match a filter.",
		&storeRefsCmd,
	)
	if err != nil {
		log.Fatal(err)
	}
}

type StoreCmd struct {
	Type   string `short:"t" long:"type" description:"the (multi-)repo store type to use (RepoStore, MultiRepoStore, etc.)" default:"RepoStore"`
	Root   string `short:"r" long:"root" description:"the root of the store (repo clone dir for RepoStore, global path for MultiRepoStore, etc.)" default:".srclib-store"`
	Config string `long:"config" description:"(rarely used) JSON-encoded config for extra config, specific to each store type"`
}

var storeCmd StoreCmd

func (c *StoreCmd) Execute(args []string) error { return nil }

// store returns the store specified by StoreCmd's Type and Root
// options.
func (c *StoreCmd) store() (interface{}, error) {
	var fs rwvfs.FileSystem
	// Attempt to parse Root as a url, and fallback to creating an
	// OS file system if it isn't.
	if u, err := url.Parse(c.Root); err == nil && strings.HasSuffix(u.Host, "amazonaws.com") {
		fs = s3vfs.S3(u, nil)
	} else {
		fs = rwvfs.OS(c.Root)
	}

	type createParents interface {
		CreateParentDirs(bool)
	}
	if fs, ok := fs.(createParents); ok {
		fs.CreateParentDirs(true)
	}

	switch c.Type {
	case "RepoStore":
		return store.NewFSRepoStore(fs), nil
	case "MultiRepoStore":
		var conf *store.FSMultiRepoStoreConf
		if c.Config != "" {
			// Only really allows configuring EvenlyDistributedRepoPaths right now.
			var conf2 struct {
				RepoPaths string
			}
			if err := json.Unmarshal([]byte(c.Config), &conf2); err != nil {
				return nil, fmt.Errorf("--config %q: %s", c.Config, err)
			}
			if conf2.RepoPaths == "EvenlyDistributedRepoPaths" {
				conf = &store.FSMultiRepoStoreConf{RepoPaths: &store.EvenlyDistributedRepoPaths{}}
			}
		}
		return store.NewFSMultiRepoStore(rwvfs.Walkable(fs), conf), nil
	default:
		return nil, fmt.Errorf("unrecognized store --type value: %q (valid values are RepoStore, MultiRepoStore)", c.Type)
	}
}

type StoreImportCmd struct {
	DryRun bool `short:"n" long:"dry-run" description:"print what would be done but don't do anything"`

	Sample           bool `long:"sample" description:"(sample data) import sample data, not .srclib-cache data"`
	SampleDefs       int  `long:"sample-defs" description:"(sample data) number of sample defs to import" default:"100"`
	SampleRefs       int  `long:"sample-refs" description:"(sample data) number of sample refs to import" default:"100"`
	SampleImportOnly bool `long:"sample-import-only" description:"(sample data) only import, don't demonstrate listing data"`

	Repo     string `long:"repo" description:"only import for this repo"`
	Unit     string `long:"unit" description:"only import source units with this name"`
	UnitType string `long:"unit-type" description:"only import source units with this type"`
	CommitID string `long:"commit" description:"commit ID of commit whose data to import"`

	RemoteBuildDataRepo string `long:"remote-build-data-repo" description:"the repo whose remote build data to import (defaults to '--repo' option value)"`
	RemoteBuildData     bool   `long:"remote-build-data" description:"import remote build data (not the local .srclib-cache build data)"`
}

var storeImportCmd StoreImportCmd

func (c *StoreImportCmd) Execute(args []string) error {
	start := time.Now()

	s, err := storeCmd.store()
	if err != nil {
		return err
	}

	if c.Sample {
		return c.sample(s)
	}

	lrepo, err := openLocalRepo()
	if err != nil {
		return err
	}

	if c.RemoteBuildDataRepo == "" {
		c.RemoteBuildDataRepo = c.Repo
	}
	bdfs, label, err := getBuildDataFS(!c.RemoteBuildData, c.RemoteBuildDataRepo, c.CommitID)
	if err != nil {
		return err
	}
	if GlobalOpt.Verbose {
		log.Printf("# Importing build data for %s (commit %s) from %s", c.Repo, c.CommitID, label)
	}

	// Traverse the build data directory for this repo and commit to
	// create the makefile that lists the targets (which are the data
	// files we will import).
	treeConfig, err := config.ReadCached(bdfs)
	if err != nil {
		return err
	}
	mf, err := plan.CreateMakefile(".", nil, lrepo.VCSType, treeConfig, plan.Options{NoCache: true})
	if err != nil {
		return err
	}

	for _, rule := range mf.Rules {
		if c.Unit != "" || c.UnitType != "" {
			type ruleForSourceUnit interface {
				SourceUnit() *unit.SourceUnit
			}
			if rule, ok := rule.(ruleForSourceUnit); ok {
				u := rule.SourceUnit()
				if (c.Unit != "" && u.Name != c.Unit) || (c.UnitType != "" && u.Type != c.UnitType) {
					continue
				}
			} else {
				// Skip all non-source-unit rules if --unit or
				// --unit-type are specified.
				continue
			}
		}

		switch rule := rule.(type) {
		case *grapher.GraphUnitRule:
			var data graph.Output
			if err := readJSONFileFS(bdfs, rule.Target(), &data); err != nil {
				return err
			}
			if c.DryRun || GlobalOpt.Verbose {
				log.Printf("# Importing graph data (%d defs, %d refs, %d docs, %d anns) for unit %s %s", len(data.Defs), len(data.Refs), len(data.Docs), len(data.Anns), rule.Unit.Type, rule.Unit.Name)
				if c.DryRun {
					continue
				}
			}

			// HACK: Transfer docs to [def].Docs.
			docsByPath := make(map[string]*graph.Doc, len(data.Docs))
			for _, doc := range data.Docs {
				docsByPath[doc.Path] = doc
			}
			for _, def := range data.Defs {
				if doc, present := docsByPath[def.Path]; present {
					def.Docs = append(def.Docs, graph.DefDoc{Format: doc.Format, Data: doc.Data})
				}
			}

			switch imp := s.(type) {
			case store.RepoImporter:
				if err := imp.Import(c.CommitID, rule.Unit, data); err != nil {
					return err
				}
			case store.MultiRepoImporter:
				if err := imp.Import(c.Repo, c.CommitID, rule.Unit, data); err != nil {
					return err
				}
			default:
				return fmt.Errorf("store (type %T) does not implement importing", s)
			}
		}
	}

	log.Printf("# Import completed in %s.", time.Since(start))
	return nil
}

// sample imports sample data (when the --sample option is given).
func (c *StoreImportCmd) sample(s interface{}) error {
	dataString := []byte(`"abcdabcdabcdabcdabcdcdabcdabcdabcdabcdabcdabcdabcdabcdcdabcdabcdabcdabcdabcdabcdabcdabcdcdabcdabcdabcdabcdabcdabcdabcdabcdcdabcdabcdabcdabcdabcdabcdabcdabcdcdabcdabcdabcdabcdabcdabcdabcdabcdcdabcdabcdabcdabcdabcdabcdabcdabcdcdabcdabcdabcd"`)
	makeGraphData := func(numDefs, numRefs int) *graph.Output {
		defs := make([]graph.Def, numDefs)
		refs := make([]graph.Ref, numRefs)

		data := graph.Output{
			Defs: make([]*graph.Def, numDefs),
			Refs: make([]*graph.Ref, numRefs),
		}

		var wg sync.WaitGroup

		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < numDefs; i++ {
				defs[i] = graph.Def{
					DefKey:   graph.DefKey{Path: fmt.Sprintf("def-path-%d", i)},
					Name:     fmt.Sprintf("def-name-%d", i),
					Kind:     "mykind",
					DefStart: uint32((i % 53) * 37),
					DefEnd:   uint32((i%53)*37 + (i % 20)),
					File:     fmt.Sprintf("dir%d/subdir%d/subsubdir%d/file-%d.foo", i%5, i%3, i%7, i%11),
					Exported: i%5 == 0,
					Local:    i%3 == 0,
					Data:     dataString,
				}
			}
		}()

		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < numRefs; i++ {
				refs[i] = graph.Ref{
					DefPath: fmt.Sprintf("ref-path-%d", i),
					Def:     i%5 == 0,
					Start:   uint32((i % 51) * 39),
					End:     uint32((i%51)*37 + (int(i) % 18)),
					File:    fmt.Sprintf("dir%d/subdir%d/subsubdir%d/file-%d.foo", i%3, i%5, i%7, i%11),
				}
				if i%3 == 0 {
					refs[i].DefUnit = fmt.Sprintf("def-unit-%d", i%17)
					refs[i].DefUnitType = fmt.Sprintf("def-unit-type-%d", i%3)
					if i%7 == 0 {
						refs[i].DefRepo = fmt.Sprintf("def-repo-%d", i%13)
					}
				}
			}
		}()

		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := range defs {
				data.Defs[i] = &defs[i]
			}
			for i := range refs {
				data.Refs[i] = &refs[i]
			}
		}()

		wg.Wait()
		return &data
	}

	start := time.Now()
	log.Printf("Making sample data (%d defs, %d refs)", c.SampleDefs, c.SampleRefs)
	data := makeGraphData(c.SampleDefs, c.SampleRefs)
	unit := &unit.SourceUnit{Type: "MyUnitType", Name: "MyUnit"}
	files := map[string]struct{}{}
	for _, def := range data.Defs {
		files[def.File] = struct{}{}
	}
	for _, ref := range data.Refs {
		files[ref.File] = struct{}{}
	}
	for f := range files {
		unit.Files = append(unit.Files, f)
	}
	if d := time.Since(start); d > time.Millisecond*250 {
		log.Printf("Done making sample data (took %s).", d)
	}

	size, err := store.Codec.NewEncoder(ioutil.Discard).Encode(data)
	if err != nil {
		return err
	}
	log.Printf("Encoded data is %s", bytesString(size))

	commitID := strings.Repeat("f", 40)
	log.Printf("Importing %d defs and %d refs into the source unit %+v at commit %s", len(data.Defs), len(data.Refs), unit.ID2(), commitID)
	start = time.Now()
	switch imp := s.(type) {
	case store.RepoImporter:
		if err := imp.Import(commitID, unit, *data); err != nil {
			return err
		}
	case store.MultiRepoImporter:
		repo := "example.com/my/repo"
		log.Printf(" - repo %s", repo)
		if err := imp.Import(repo, commitID, unit, *data); err != nil {
			return err
		}
	default:
		return fmt.Errorf("store (type %T) does not implement importing", s)
	}
	log.Printf("Import took %s (~%s per def/ref)", time.Since(start), time.Duration(int64(time.Since(start))/int64(len(data.Defs)+len(data.Refs))))

	if c.SampleImportOnly {
		return nil
	}

	log.Println()
	log.Printf("Running some commands to list sample data")

	runCmd := func(args ...string) error {
		start := time.Now()
		var b bytes.Buffer
		c := exec.Command(args[0], args[1:]...)
		c.Stdout = &b
		c.Stderr = &b
		log.Println()
		log.Println(strings.Join(c.Args, " "))
		if err := c.Run(); err != nil {
			return fmt.Errorf("command %v failed\n\noutput was:\n%s", c.Args, b.String())
		}
		if GlobalOpt.Verbose {
			log.Println(b.String())
		} else {
			log.Printf("-> printed %d lines of output (run with `src -v` to view)", bytes.Count(b.Bytes(), []byte{'\n'}))
		}
		log.Printf("-> took %s", time.Since(start))
		return nil
	}
	if err := runCmd("src", "store", "versions"); err != nil {
		return err
	}
	if err := runCmd("src", "store", "units"); err != nil {
		return err
	}
	if err := runCmd("src", "store", "units", "--file", data.Defs[len(data.Defs)/2+1].File); err != nil {
		return err
	}
	if err := runCmd("src", "store", "units", "--file", data.Refs[len(data.Refs)/2+1].File); err != nil {
		return err
	}
	if err := runCmd("src", "store", "defs", "--file", data.Defs[len(data.Defs)/3+1].File); err != nil {
		return err
	}
	if err := runCmd("src", "store", "refs", "--file", data.Refs[len(data.Refs)/2+1].File); err != nil {
		return err
	}

	return nil
}

// countingWriter wraps an io.Writer, counting the number of bytes
// written.
type countingWriter struct {
	io.Writer
	n uint64
}

func (cr *countingWriter) Write(p []byte) (n int, err error) {
	n, err = cr.Writer.Write(p)
	cr.n += uint64(n)
	return
}

type storeIndexCriteria struct {
	Repo     string `long:"repo" description:"only indexes for this repo"`
	CommitID string `long:"commit" description:"only indexes for this commit ID"`
	UnitType string `long:"unit-type" description:"only indexes for this source unit type"`
	Unit     string `long:"unit" description:"only indexes for this source unit name"`
	Name     string `long:"name" description:"only indexes whose name contains this substring"`
	Type     string `long:"type" description:"only indexes whose Go type contains this substring"`

	Stale    bool `long:"stale" description:"only stale indexes"`
	NotStale bool `long:"not-stale" description:"only non-stale indexes"`
}

func (c storeIndexCriteria) IndexCriteria() store.IndexCriteria {
	crit := store.IndexCriteria{
		Repo:     c.Repo,
		CommitID: c.CommitID,
		Name:     c.Name,
		Type:     c.Type,
	}
	if c.Stale && c.NotStale {
		log.Fatal("must specify exactly one of --stale and --not-stale")
	}
	if c.Stale {
		t := true
		crit.Stale = &t
	}
	if c.NotStale {
		f := false
		crit.Stale = &f
	}
	if c.UnitType != "" || c.Unit != "" {
		crit.Unit = &unit.ID2{Type: c.UnitType, Name: c.Unit}
		if crit.Unit.Type == "" || crit.Unit.Name == "" {
			log.Fatal("must specify either both or neither of --unit-type and --unit (to filter by source unit)")
		}
	}
	return crit
}

// doStoreIndexesCmd is invoked by both StoreIndexesCmd.Execute and
// StoreBuildIndexesCmd.Execute.
func doStoreIndexesCmd(crit store.IndexCriteria, opt storeIndexOptions, f func(interface{}, store.IndexCriteria, chan<- store.IndexStatus) ([]store.IndexStatus, error)) error {
	s, err := storeCmd.store()
	if err != nil {
		return err
	}

	hasError := false
	done := make(chan struct{})
	indexChan := make(chan store.IndexStatus)
	switch opt.Output {
	case "json":
		go func() {
			for x := range indexChan {
				PrintJSON(x, "")
			}
			done <- struct{}{}
		}()
	case "text":
		_, isMultiRepo := s.(store.MultiRepoStore)
		var repoTab string
		if isMultiRepo {
			repoTab = "\t"
		}

		go func() {
			var lastRepo, lastCommitID string
			var lastUnit *unit.ID2
			for x := range indexChan {
				if isMultiRepo {
					if x.Repo != lastRepo {
						if lastRepo != "" {
							fmt.Println()
						}
						fmt.Println(x.Repo)
					}
				}
				if x.CommitID != lastCommitID {
					fmt.Print(repoTab, x.CommitID, "\n")
				}
				if x.Unit != lastUnit && x.Unit != nil {
					if x.Repo == lastRepo && x.CommitID == lastCommitID {
						fmt.Println()
					}
					fmt.Print(repoTab, "\t", x.Unit.Name, " ", x.Unit.Type, "\n")
				}

				if x.Unit != nil {
					fmt.Print("\t")
				}

				fmt.Print(repoTab, "\t")
				fmt.Printf("%s (%s) ", x.Name, x.Type)
				if x.Stale {
					fmt.Print("STALE ")
				}
				if x.Size != 0 {
					fmt.Print(bytesString(uint64(x.Size)), " ")
				}
				if x.Error != "" {
					fmt.Printf("(ERROR: %s) ", x.Error)
					hasError = true
				}
				if x.BuildError != "" {
					fmt.Printf("(BUILD ERROR: %s) ", x.BuildError)
					hasError = true
				}
				if x.BuildDuration != 0 {
					fmt.Printf("- build took %s ", x.BuildDuration)
				}
				fmt.Println()

				lastRepo = x.Repo
				lastCommitID = x.CommitID
				lastUnit = x.Unit
			}
			done <- struct{}{}
		}()
	default:
		return fmt.Errorf("unexpected --output value: %q", opt.Output)
	}

	_, err = f(s, crit, indexChan)
	defer func() {
		close(indexChan)
		<-done
	}()
	if err != nil {
		return err
	}
	if hasError {
		return errors.New("\nindex listing or index building errors occurred (see above)")
	}
	return nil
}

type storeIndexOptions struct {
	Output string `short:"o" long:"output" description:"output format (text|json)" default:"text"`
}

type StoreIndexesCmd struct {
	storeIndexCriteria
	storeIndexOptions
}

var storeIndexesCmd StoreIndexesCmd

func (c *StoreIndexesCmd) Execute(args []string) error {
	return doStoreIndexesCmd(c.IndexCriteria(), c.storeIndexOptions, store.Indexes)
}

type StoreIndexCmd struct {
	storeIndexCriteria
	storeIndexOptions
}

var storeIndexCmd StoreIndexCmd

func (c *StoreIndexCmd) Execute(args []string) error {
	return doStoreIndexesCmd(c.IndexCriteria(), c.storeIndexOptions, store.BuildIndexes)
}

type StoreReposCmd struct {
	IDContains string `short:"i" long:"id-contains" description:"filter to repos whose ID contains this substring"`
}

func (c *StoreReposCmd) filters() []store.RepoFilter {
	var fs []store.RepoFilter
	if c.IDContains != "" {
		fs = append(fs, store.RepoFilterFunc(func(repo string) bool { return strings.Contains(repo, c.IDContains) }))
	}
	return fs
}

var storeReposCmd StoreReposCmd

func (c *StoreReposCmd) Execute(args []string) error {
	s, err := storeCmd.store()
	if err != nil {
		return err
	}

	mrs, ok := s.(store.MultiRepoStore)
	if !ok {
		return fmt.Errorf("store (type %T) does not implement listing repositories", s)
	}

	repos, err := mrs.Repos(c.filters()...)
	if err != nil {
		return err
	}
	for _, repo := range repos {
		fmt.Println(repo)
	}
	return nil
}

type StoreVersionsCmd struct {
	Repo           string `long:"repo"`
	CommitIDPrefix string `long:"commit" description:"commit ID prefix"`
}

func (c *StoreVersionsCmd) filters() []store.VersionFilter {
	var fs []store.VersionFilter
	if c.Repo != "" {
		fs = append(fs, store.ByRepo(c.Repo))
	}
	if c.CommitIDPrefix != "" {
		fs = append(fs, store.VersionFilterFunc(func(version *store.Version) bool {
			return strings.HasPrefix(version.CommitID, c.CommitIDPrefix)
		}))
	}
	return fs
}

var storeVersionsCmd StoreVersionsCmd

func (c *StoreVersionsCmd) Execute(args []string) error {
	s, err := storeCmd.store()
	if err != nil {
		return err
	}

	rs, ok := s.(store.RepoStore)
	if !ok {
		return fmt.Errorf("store (type %T) does not implement listing versions", s)
	}

	versions, err := rs.Versions(c.filters()...)
	if err != nil {
		return err
	}
	for _, version := range versions {
		if version.Repo != "" {
			fmt.Print(version.Repo, "\t")
		}
		fmt.Println(version.CommitID)
	}
	return nil
}

type StoreUnitsCmd struct {
	Type     string `long:"type" `
	Name     string `long:"name"`
	CommitID string `long:"commit"`
	Repo     string `long:"repo"`

	File string `long:"file" description:"filter by units whose Files list contains this file"`
}

func (c *StoreUnitsCmd) filters() []store.UnitFilter {
	var fs []store.UnitFilter
	if c.Type != "" && c.Name != "" {
		fs = append(fs, store.ByUnits(unit.ID2{Type: c.Type, Name: c.Name}))
	}
	if (c.Type != "" && c.Name == "") || (c.Type == "" && c.Name != "") {
		log.Fatal("must specify either both or neither of --type and --name (to filter by source unit)")
	}
	if c.CommitID != "" {
		fs = append(fs, store.ByCommitID(c.CommitID))
	}
	if c.Repo != "" {
		fs = append(fs, store.ByRepo(c.Repo))
	}
	if c.File != "" {
		fs = append(fs, store.ByFiles(path.Clean(c.File)))
	}
	return fs
}

var storeUnitsCmd StoreUnitsCmd

func (c *StoreUnitsCmd) Execute(args []string) error {
	s, err := storeCmd.store()
	if err != nil {
		return err
	}

	ts, ok := s.(store.TreeStore)
	if !ok {
		return fmt.Errorf("store (type %T) does not implement listing source units", s)
	}

	units, err := ts.Units(c.filters()...)
	if err != nil {
		return err
	}
	PrintJSON(units, "  ")
	return nil
}

type StoreDefsCmd struct {
	Repo           string `long:"repo"`
	Path           string `long:"path"`
	UnitType       string `long:"unit-type" `
	Unit           string `long:"unit"`
	File           string `long:"file"`
	FilePathPrefix string `long:"file-path-prefix"`
	CommitID       string `long:"commit"`

	NamePrefix string `long:"name-prefix"`

	Limit int `short:"n" long:"limit" description:"max results to return (0 for all)"`
}

func (c *StoreDefsCmd) filters() []store.DefFilter {
	var fs []store.DefFilter
	if c.UnitType != "" && c.Unit != "" {
		fs = append(fs, store.ByUnits(unit.ID2{Type: c.UnitType, Name: c.Unit}))
	}
	if (c.UnitType != "" && c.Unit == "") || (c.UnitType == "" && c.Unit != "") {
		log.Fatal("must specify either both or neither of --unit-type and --unit (to filter by source unit)")
	}
	if c.CommitID != "" {
		fs = append(fs, store.ByCommitID(c.CommitID))
	}
	if c.Repo != "" {
		fs = append(fs, store.ByRepo(c.Repo))
	}
	if c.Path != "" {
		fs = append(fs, store.ByDefPath(c.Path))
	}
	if c.File != "" {
		fs = append(fs, store.ByFiles(path.Clean(c.File)))
	}
	if c.FilePathPrefix != "" {
		fs = append(fs, store.ByFiles(path.Clean(c.FilePathPrefix)))
	}
	if c.NamePrefix != "" {
		fs = append(fs, store.DefFilterFunc(func(def *graph.Def) bool {
			return strings.HasPrefix(def.Name, c.NamePrefix)
		}))
	}
	if c.Limit != 0 {
		fs = append(fs, store.Limit(c.Limit))
	}
	return fs
}

var storeDefsCmd StoreDefsCmd

func (c *StoreDefsCmd) Execute(args []string) error {
	s, err := storeCmd.store()
	if err != nil {
		return err
	}

	us, ok := s.(store.UnitStore)
	if !ok {
		return fmt.Errorf("store (type %T) does not implement listing defs", s)
	}

	defs, err := us.Defs(c.filters()...)
	if err != nil {
		return err
	}
	PrintJSON(defs, "  ")
	return nil
}

type StoreRefsCmd struct {
	Repo     string `long:"repo"`
	UnitType string `long:"unit-type" `
	Unit     string `long:"unit"`
	File     string `long:"file"`
	CommitID string `long:"commit"`

	Start uint32 `long:"start"`
	End   uint32 `long:"end"`

	DefRepo     string `long:"def-repo"`
	DefUnitType string `long:"def-unit-type" `
	DefUnit     string `long:"def-unit"`
	DefPath     string `long:"def-path"`
}

func (c *StoreRefsCmd) filters() []store.RefFilter {
	var fs []store.RefFilter
	if c.UnitType != "" && c.Unit != "" {
		fs = append(fs, store.ByUnits(unit.ID2{Type: c.UnitType, Name: c.Unit}))
	}
	if (c.UnitType != "" && c.Unit == "") || (c.UnitType == "" && c.Unit != "") {
		log.Fatal("must specify either both or neither of --unit-type and --unit (to filter by source unit)")
	}
	if c.CommitID != "" {
		fs = append(fs, store.ByCommitID(c.CommitID))
	}
	if c.Repo != "" {
		fs = append(fs, store.ByRepo(c.Repo))
	}
	if c.File != "" {
		fs = append(fs, store.ByFiles(path.Clean(c.File)))
	}
	if c.Start != 0 {
		fs = append(fs, store.RefFilterFunc(func(ref *graph.Ref) bool {
			return ref.Start >= c.Start
		}))
	}
	if c.End != 0 {
		fs = append(fs, store.RefFilterFunc(func(ref *graph.Ref) bool {
			return ref.End <= c.End
		}))
	}
	if c.DefRepo != "" && c.DefUnitType != "" && c.DefUnit != "" && c.DefPath != "" {
		fs = append(fs, store.ByRefDef(graph.RefDefKey{
			DefRepo:     c.DefRepo,
			DefUnitType: c.DefUnitType,
			DefUnit:     c.DefUnit,
			DefPath:     c.DefPath,
		}))
	}
	if (c.DefRepo != "" || c.DefUnitType != "" || c.DefUnit != "" || c.DefPath != "") && (c.DefRepo == "" || c.DefUnitType == "" || c.DefUnit == "" || c.DefPath == "") {
		log.Fatal("must specify either all or neither of --def-repo, --def-unit-type, --def-unit, and --def-path (to filter by ref target def)")
	}
	return fs
}

var storeRefsCmd StoreRefsCmd

func (c *StoreRefsCmd) Execute(args []string) error {
	s, err := storeCmd.store()
	if err != nil {
		return err
	}

	us, ok := s.(store.UnitStore)
	if !ok {
		return fmt.Errorf("store (type %T) does not implement listing refs", s)
	}

	refs, err := us.Refs(c.filters()...)
	if err != nil {
		return err
	}
	PrintJSON(refs, "  ")
	return nil
}
