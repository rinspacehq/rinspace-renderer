package main

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"

	"github.com/rinspacehq/rinspace-renderer/api/internal/contracts"
	"github.com/rinspacehq/rinspace-renderer/api/internal/publicationpdf"
)

func runMarkdownPDF(ctx context.Context, input io.Reader, output io.Writer) error {
	var request struct {
		Bundle contracts.DocumentBundle `json:"bundle"`
		Book   bool                     `json:"book"`
		Images map[string]string        `json:"images"`
	}
	body, err := io.ReadAll(io.LimitReader(input, publicationpdf.MaxPrintInputBytes+1))
	if err != nil {
		return err
	}
	if len(body) > publicationpdf.MaxPrintInputBytes {
		return errors.New("Markdown PDF request exceeds 64 MiB")
	}
	if err := json.Unmarshal(body, &request); err != nil {
		return errors.New("invalid Markdown PDF request")
	}
	printer := publicationpdf.BrowserPrinter{
		Node:     os.Getenv("RIN_RENDERER_MARKDOWN_NODE_BIN"),
		Script:   os.Getenv("RIN_RENDERER_MARKDOWN_PDF_SCRIPT"),
		Chromium: os.Getenv("RIN_RENDERER_MARKDOWN_PDF_CHROMIUM"),
	}
	pdf, _, err := printer.Print(ctx, request.Bundle, request.Book, func(_ context.Context, reference publicationpdf.ImageReference) (publicationpdf.ResolvedImage, error) {
		if image := request.Images[reference.Source]; image != "" {
			return publicationpdf.ResolvedImage{DataURL: image}, nil
		}
		return publicationpdf.ResolvedImage{}, errors.New("missing staged print image")
	})
	if err != nil {
		return err
	}
	_, err = output.Write(pdf)
	return err
}
