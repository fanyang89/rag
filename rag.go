package main

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/cockroachdb/errors"
	"github.com/gobwas/glob"
	"github.com/goccy/go-json"
	"github.com/jedib0t/go-pretty/v6/table"
	"github.com/joho/godotenv"
	"github.com/openai/openai-go"
	"github.com/openai/openai-go/option"
	"github.com/rs/zerolog"
	"github.com/rs/zerolog/log"
	"github.com/urfave/cli/v3"

	"github.com/fioepq9/pzlog"

	"github.com/fanyang89/rag/v1"
)

var cmd = &cli.Command{
	Name: "rag",
	Commands: []*cli.Command{
		serveCmd,
		scanCmd,
		computeCmd,
		searchCmd,
		getChunkCmd,
		generateScriptCmd,
	},
}

var trimSpace = cli.StringConfig{TrimSpace: true}

var generateScriptCmd = &cli.Command{
	Name:    "generate-script",
	Aliases: []string{"gen", "generate"},
	Arguments: []cli.Argument{
		&cli.StringArg{Name: "path", Config: trimSpace},
	},
	Flags: []cli.Flag{
		&cli.StringFlag{Name: "rag-tools", Aliases: []string{"t", "tools"}, Config: trimSpace},
		&cli.StringFlag{Name: "output", Aliases: []string{"o"}, Value: "-", Config: trimSpace},
		&cli.BoolFlag{Name: "shebang", Value: true},
		&cli.BoolFlag{Name: "with-pueue", Value: false},
	},
	Action: func(ctx context.Context, command *cli.Command) error {
		var err error

		withPueue := command.Bool("with-pueue")
		path := command.StringArg("path")
		if path == "" {
			return errors.New("path is required")
		}
		toolPath := command.String("rag-tools")

		var w io.Writer
		if outputPath := command.String("output"); outputPath == "-" {
			w = os.Stdout
		} else {
			f, err := os.Create(outputPath)
			if err != nil {
				return err
			}
			defer func() { _ = f.Close() }()
			w = f
		}

		if command.Bool("shebang") {
			_, err = fmt.Fprintln(w, "#!/bin/bash")
			if err != nil {
				return err
			}
		}

		_, err = fmt.Fprintln(w, "trap 'exit' INT")
		if err != nil {
			return err
		}

		err = filepath.WalkDir(path, func(path string, d fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if d.IsDir() {
				return nil
			}
			if !strings.HasSuffix(path, ".pdf") ||
				strings.HasSuffix(path, "_layout.pdf") ||
				strings.HasSuffix(path, "_origin.pdf") ||
				strings.HasSuffix(path, "_span.pdf") {
				return nil
			}

			path, err = filepath.Abs(path)
			if err != nil {
				panic(err)
			}
			baseDir := filepath.Dir(path)
			fileNameExt := filepath.Base(path)
			fileName := strings.TrimSuffix(fileNameExt, filepath.Ext(fileNameExt))
			markdownFilePath := filepath.Join(baseDir, fileName, "auto", fileName+".md")

			toolArg := " "
			ragCliPath := "rag.py"
			if toolPath != "" {
				toolArg = fmt.Sprintf(" --project %s ", toolPath)
				ragCliPath = filepath.Join(toolPath, "rag.py")
			}

			if withPueue {
				_, err = fmt.Fprintf(w, "pueue add -- \"")
				if err != nil {
					return err
				}
			}

			_, err = fmt.Fprintf(w, "uv run%smineru --source modelscope -p '%s' -o '%s'",
				toolArg, path, baseDir)
			if err != nil {
				return err
			}

			_, err = fmt.Fprintf(io.Discard, "uv run%s%s chunking '%s' --output '%s.chunks.json'",
				toolArg, ragCliPath, markdownFilePath, markdownFilePath)

			if withPueue {
				_, err = fmt.Fprintf(w, "\"\n")
				if err != nil {
					return err
				}
			}
			return err
		})
		if err != nil {
			return err
		}
		_, err = fmt.Fprintln(w)
		return err
	},
}

var serveCmd = &cli.Command{
	Name:  "serve",
	Usage: "start rag server",
	Flags: []cli.Flag{
		&cli.StringFlag{
			Name:    "bind",
			Aliases: []string{"a", "l"},
			Value:   ":5000",
		},
		&cli.StringFlag{
			Name:    "dsn",
			Sources: cli.ValueSourceChain{Chain: []cli.ValueSource{cli.EnvVar("RAG_DSN")}},
		},
		&cli.StringFlag{
			Name:    "base_url",
			Sources: cli.ValueSourceChain{Chain: []cli.ValueSource{cli.EnvVar("EMBEDDING_BASE_URL")}},
		},
		&cli.StringFlag{
			Name:    "model",
			Sources: cli.ValueSourceChain{Chain: []cli.ValueSource{cli.EnvVar("EMBEDDING_MODEL")}},
		},
	},
	Action: func(ctx context.Context, command *cli.Command) error {
		baseURL := command.String("base_url")
		model := command.String("model")
		dsn := command.String("dsn")

		db, err := rag.OpenDB(dsn)
		if err != nil {
			return err
		}

		client := openai.NewClient(option.WithBaseURL(baseURL))
		r := &rag.RAG{DB: db, Client: &client, Model: model}

		s := rag.NewServer(r)
		go func() {
			select {
			case <-ctx.Done():
				closeCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
				defer cancel()
				_ = s.Shutdown(closeCtx)
			}
		}()
		err = s.Start(command.String("bind"))
		if err == nil || errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return err
	},
}

var scanCmd = &cli.Command{
	Name:  "scan",
	Usage: "Scan directories for files and upsert them into the database",
	Arguments: []cli.Argument{
		&cli.StringArg{Name: "path"},
	},
	Flags: []cli.Flag{
		&cli.StringFlag{
			Name:    "glob",
			Aliases: []string{"g"},
			Value:   "*.chunks.json",
		},
		&cli.StringFlag{
			Name: "dsn",
			Sources: cli.ValueSourceChain{
				Chain: []cli.ValueSource{
					cli.EnvVar("RAG_DSN"),
				},
			},
		},
	},
	Action: func(ctx context.Context, command *cli.Command) error {
		path := command.StringArg("path")
		if path == "" {
			return errors.New("path is required")
		}
		g, err := glob.Compile(command.String("glob"))
		if err != nil {
			return err
		}

		db, err := rag.OpenDB(command.String("dsn"))
		if err != nil {
			return err
		}

		r := rag.RAG{DB: db}

		return filepath.WalkDir(path, func(path string, d fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if d.IsDir() {
				return nil
			}
			if !g.Match(d.Name()) {
				return nil
			}

			buf, err := os.ReadFile(path)
			if err != nil {
				return err
			}

			decoder := json.NewDecoder(bytes.NewReader(buf))
			decoder.DisallowUnknownFields()
			var chunks rag.Document
			err = decoder.Decode(&chunks)
			if err != nil {
				return err
			}

			return r.UpsertDocumentChunks(&chunks)
		})
	},
}

var computeCmd = &cli.Command{
	Name:  "compute",
	Usage: "Compute embeddings for files in the database",
	Flags: []cli.Flag{
		&cli.StringFlag{
			Name:    "dsn",
			Sources: cli.ValueSourceChain{Chain: []cli.ValueSource{cli.EnvVar("RAG_DSN")}},
		},
		&cli.StringFlag{
			Name:    "base_url",
			Sources: cli.ValueSourceChain{Chain: []cli.ValueSource{cli.EnvVar("EMBEDDING_BASE_URL")}},
		},
		&cli.StringFlag{
			Name:    "model",
			Sources: cli.ValueSourceChain{Chain: []cli.ValueSource{cli.EnvVar("EMBEDDING_MODEL")}},
		},
		&cli.BoolFlag{
			Name:  "force",
			Value: false,
		},
	},
	Action: func(ctx context.Context, command *cli.Command) error {
		baseURL := command.String("base_url")
		model := command.String("model")
		dsn := command.String("dsn")
		force := command.Bool("force")

		db, err := rag.OpenDB(dsn)
		if err != nil {
			return err
		}

		client := openai.NewClient(option.WithBaseURL(baseURL))
		r := rag.RAG{DB: db, Client: &client, Model: model}

		return r.ComputeEmbeddings(ctx, !force)
	},
}

var searchCmd = &cli.Command{
	Name:  "search",
	Usage: "Search for documents",
	Arguments: []cli.Argument{
		&cli.StringArg{Name: "query", Config: cli.StringConfig{TrimSpace: true}},
	},
	Flags: []cli.Flag{
		&cli.StringFlag{
			Name:    "dsn",
			Sources: cli.ValueSourceChain{Chain: []cli.ValueSource{cli.EnvVar("RAG_DSN")}},
		},
		&cli.StringFlag{
			Name:    "base_url",
			Sources: cli.ValueSourceChain{Chain: []cli.ValueSource{cli.EnvVar("EMBEDDING_BASE_URL")}},
		},
		&cli.StringFlag{
			Name:    "model",
			Sources: cli.ValueSourceChain{Chain: []cli.ValueSource{cli.EnvVar("EMBEDDING_MODEL")}},
		},
		&cli.IntFlag{
			Name:  "limit",
			Value: 3,
		},
	},
	Action: func(ctx context.Context, command *cli.Command) error {
		query := command.StringArg("query")
		if query == "" {
			return errors.New("query is required")
		}

		baseURL := command.String("base_url")
		model := command.String("model")
		dsn := command.String("dsn")
		limit := command.Int("limit")

		db, err := rag.OpenDB(dsn)
		if err != nil {
			return err
		}

		client := openai.NewClient(option.WithBaseURL(baseURL))
		r := rag.RAG{DB: db, Client: &client, Model: model}

		chunks, err := r.QueryDocuments(ctx, query, limit)
		if err != nil {
			return err
		}

		tw := table.NewWriter()
		tw.AppendHeader(table.Row{"ID", "Raw document", "Chunk ID"})
		for _, chunk := range chunks {
			tw.AppendRow(table.Row{
				chunk.ID,
				chunk.RawDocument,
				chunk.ChunkID,
			})
		}
		fmt.Println(tw.Render())
		return nil
	},
}

var getChunkCmd = &cli.Command{
	Name: "get",
	Arguments: []cli.Argument{
		&cli.StringArg{Name: "id", Config: cli.StringConfig{TrimSpace: true}},
	},
	Flags: []cli.Flag{
		&cli.StringFlag{
			Name:    "dsn",
			Sources: cli.ValueSourceChain{Chain: []cli.ValueSource{cli.EnvVar("RAG_DSN")}},
		},
	},
	Action: func(ctx context.Context, command *cli.Command) error {
		id := command.StringArg("id")
		if id == "" {
			return errors.New("id is required")
		}
		dsn := command.String("dsn")
		db, err := rag.OpenDB(dsn)
		if err != nil {
			return err
		}

		r := rag.RAG{DB: db}
		c, err := r.GetDocumentChunk(id)
		fmt.Println(c.Text)
		return nil
	},
}

func main() {
	_ = godotenv.Load(".env")

	zerolog.SetGlobalLevel(zerolog.InfoLevel)
	log.Logger = zerolog.New(pzlog.NewPtermWriter()).With().Timestamp().Caller().Stack().Logger()

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	err := cmd.Run(ctx, os.Args)
	if err != nil {
		log.Error().Err(err).Msg("Unexpected error")
	}
}
