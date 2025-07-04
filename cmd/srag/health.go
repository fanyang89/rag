package main

import (
	"context"
	"fmt"

	"github.com/cockroachdb/errors"
	"github.com/openai/openai-go"
	"github.com/openai/openai-go/option"
	"github.com/urfave/cli/v3"

	"github.com/fanyang89/rag/v1"
)

var healthCmd = &cli.Command{
	Name:  "health",
	Usage: "Retrieve service health status",
	Flags: []cli.Flag{
		flagDSN,
		flagEmbeddingBaseURL,
		flagEmbeddingModel,
		flagRerankerBaseURL,
		flagRerankerModel,
		flagAssistantBaseURL,
		flagAssistantModel,
	},
	Action: func(ctx context.Context, command *cli.Command) error {
		dsn := command.String("dsn")
		if dsn == "" {
			return errors.New("dsn is required")
		}
		db, err := rag.OpenDB(dsn)
		if err != nil {
			return err
		}
		rawDB, err := db.DB()
		if err != nil {
			return err
		}
		err = rawDB.Ping()
		if err != nil {
			return err
		}

		embeddingBaseURL := command.String("embedding-base-url")
		if embeddingBaseURL == "" {
			return errors.New("embedding-base-url is required")
		}
		embeddingModel := command.String("embedding-model")
		if embeddingModel == "" {
			return errors.New("embedding-model is required")
		}
		embeddingClient := openai.NewClient(option.WithBaseURL(embeddingBaseURL))
		embeddingResponse, err := embeddingClient.Embeddings.New(ctx, openai.EmbeddingNewParams{
			Input: openai.EmbeddingNewParamsInputUnion{
				OfString: openai.String("Hello world"),
			},
			Model:          embeddingModel,
			EncodingFormat: openai.EmbeddingNewParamsEncodingFormatBase64,
		})
		if err != nil {
			return err
		}
		if len(embeddingResponse.Data) == 0 || len(embeddingResponse.Data[0].Embedding) == 0 {
			return errors.New("empty response")
		}

		rerankerBaseURL := command.String("reranker-base-url")
		if rerankerBaseURL == "" {
			return errors.New("reranker-base-url is required")
		}
		rerankerModel := command.String("reranker-model")
		if rerankerModel == "" {
			return errors.New("reranker-model is required")
		}
		rerankerClient := rag.NewInfinityClient(rerankerBaseURL)
		_, err = rerankerClient.Rerank(&rag.RerankRequest{
			Model:     rerankerModel,
			Query:     "Where is Munich?",
			Documents: []string{"Munich is in Germany.", "The sky is blue."},
			TopN:      3,
		})
		if err != nil {
			return err
		}

		assistantBaseURL := command.String("assistant-base-url")
		if assistantBaseURL == "" {
			return errors.New("assistant-base-url is required")
		}
		assistantModel := command.String("assistant-model")
		if assistantModel == "" {
			return errors.New("assistant-model is required")
		}
		assistantClient := openai.NewClient(option.WithBaseURL(assistantBaseURL))
		assistantResponse, err := assistantClient.Completions.New(ctx, openai.CompletionNewParams{
			Model: openai.CompletionNewParamsModel(assistantModel),
			Prompt: openai.CompletionNewParamsPromptUnion{
				OfString: openai.String("Hello world"),
			},
		})
		if err != nil {
			return err
		}
		if len(assistantResponse.Choices) == 0 || len(assistantResponse.Choices[0].Text) == 0 {
			return errors.New("empty response")
		}

		fmt.Println("OK, database/embedding/reranker/assistant are operational")
		return nil
	},
}
