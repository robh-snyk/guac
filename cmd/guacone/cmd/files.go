//
// Copyright 2022 The GUAC Authors.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package cmd

import (
	"context"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/guacsec/guac/pkg/assembler"
	"github.com/guacsec/guac/pkg/assembler/graphdb"
	"github.com/guacsec/guac/pkg/handler/collector"
	"github.com/guacsec/guac/pkg/handler/collector/file"
	"github.com/guacsec/guac/pkg/handler/processor"
	"github.com/guacsec/guac/pkg/handler/processor/process"
	"github.com/guacsec/guac/pkg/ingestor/parser"
	"github.com/guacsec/guac/pkg/logging"
	"github.com/spf13/cobra"
)

var flags = struct {
	dbAddr string
	creds  string
	realm  string
}{}

type options struct {
	dbAddr string
	user   string
	pass   string
	realm  string

	// path to folder with documents to collect
	path string
}

func init() {
	exampleCmd.PersistentFlags().StringVar(&flags.dbAddr, "db-addr", "neo4j://localhost:7687", "address to neo4j db")
	exampleCmd.PersistentFlags().StringVar(&flags.creds, "creds", "", "credentials to access neo4j in 'user:pass' format")
	exampleCmd.PersistentFlags().StringVar(&flags.realm, "realm", "neo4j", "realm to connecto graph db")
	_ = exampleCmd.MarkPersistentFlagRequired("creds")
}

var exampleCmd = &cobra.Command{
	Use:   "files [flags] file_path",
	Short: "take a folder of files and create a GUAC graph",
	Run: func(cmd *cobra.Command, args []string) {
		ctx := logging.WithLogger(context.Background())
		logger := logging.FromContext(ctx)

		opts, err := validateFlags(args)
		if err != nil {
			fmt.Printf("unable to validate flags: %v\n", err)
			_ = cmd.Help()
			os.Exit(1)
		}

		// Register collector
		fileCollector := file.NewFileCollector(ctx, opts.path, false, time.Second)
		err = collector.RegisterDocumentCollector(fileCollector, file.FileCollector)
		if err != nil {
			logger.Errorf("unable to register file collector: %v", err)
		}

		// Get pipeline of components
		processorFunc, err := getProcessor(ctx)
		if err != nil {
			logger.Errorf("error: %v", err)
			os.Exit(1)
		}
		ingestorFunc, err := getIngestor()
		if err != nil {
			logger.Errorf("error: %v", err)
			os.Exit(1)
		}
		assemblerFunc, err := getAssembler(opts)
		if err != nil {
			logger.Errorf("error: %v", err)
			os.Exit(1)
		}

		// Set emit function to go through the entire pipeline
		emit := func(d *processor.Document) {
			docTree, err := processorFunc(d)
			if err != nil {
				logger.Errorf("unable to process doc: %v, fomat: %v, document: %v", err, d.Format, d.Type)
				return
			}

			graphs, err := ingestorFunc(docTree)
			if err != nil {
				logger.Errorf("unable to ingest doc tree: %v", err)
				return
			}

			err = assemblerFunc(graphs)
			if err != nil {
				logger.Errorf("unable to assemble graphs: %v", err)
				return
			}
		}

		// Collect
		docChan, errChan, numCollectors, err := collector.Collect(ctx)
		if err != nil {
			logger.Fatal(err)
		}

		collectorsDone := 0
		for collectorsDone < numCollectors {
			select {
			case d := <-docChan:
				logger.Infof("emitting doc: %v, fomat: %v, document: %v", string(d.Blob[:10]), d.Format, d.Type)
				emit(d)
			case err = <-errChan:
				if err != nil {
					logger.Errorf("collector ended with error: %v", err)
				} else {
					logger.Info("collector ended gracefully")
				}
				collectorsDone += 1
			}
		}

		// Drain anything left in document channel
		for len(docChan) > 0 {
			d := <-docChan
			logger.Infof("emitting doc: %v, fomat: %v, document: %v", string(d.Blob[:10]), d.Format, d.Type)
			emit(d)
		}
	},
}

func validateFlags(args []string) (options, error) {
	var opts options
	credsSplit := strings.Split(flags.creds, ":")
	if len(credsSplit) != 2 {
		return opts, fmt.Errorf("creds flag not in correct format user:pass")
	}
	opts.user = credsSplit[0]
	opts.pass = credsSplit[1]
	opts.dbAddr = flags.dbAddr

	if len(args) != 1 {
		return opts, fmt.Errorf("expected positional argument for file_path")
	}
	opts.path = args[0]

	return opts, nil
}

func getProcessor(ctx context.Context) (func(*processor.Document) (processor.DocumentTree, error), error) {
	return func(d *processor.Document) (processor.DocumentTree, error) {
		return process.Process(ctx, d)
	}, nil
}
func getIngestor() (func(processor.DocumentTree) ([]assembler.Graph, error), error) {
	return func(doc processor.DocumentTree) ([]assembler.Graph, error) {
		inputs, err := parser.ParseDocumentTree(doc)
		if err != nil {
			return nil, err
		}
		return inputs, nil
	}, nil
}

func getAssembler(opts options) (func([]assembler.Graph) error, error) {
	authToken := graphdb.CreateAuthTokenWithUsernameAndPassword(opts.user, opts.pass, opts.realm)
	client, err := graphdb.NewGraphClient(opts.dbAddr, authToken)
	if err != nil {
		return nil, err
	}
	return func(gs []assembler.Graph) error {
		combined := assembler.Graph{
			Nodes: []assembler.GuacNode{},
			Edges: []assembler.GuacEdge{},
		}
		for _, g := range gs {
			combined.AppendGraph(g)
		}
		if err := assembler.StoreGraph(combined, client); err != nil {
			return err
		}

		return nil
	}, nil
}