package classify_test

import (
	"testing"

	"github.com/kamronarabi/structura/internal/classify"
)

func TestBaseImageLanguage(t *testing.T) {
	tests := map[string]string{
		"node:20-alpine":                       "javascript",
		"node":                                 "javascript",
		"bun:1":                                "javascript",
		"denoland/deno:alpine":                 "typescript",
		"golang:1.22-bookworm":                 "go",
		"python:3.12-slim":                     "python",
		"ruby:3.3":                             "ruby",
		"rust:1.78-slim":                       "rust",
		"php:8.3-fpm":                          "php",
		"elixir:1.16":                          "elixir",
		"eclipse-temurin:21-jre":               "java",
		"amazoncorretto:17":                    "java",
		"maven:3.9-eclipse-temurin-21":         "java",
		"openjdk:17-slim":                      "java",
		"mcr.microsoft.com/dotnet/aspnet:8.0":  "csharp",
		"mcr.microsoft.com/dotnet/sdk:8.0":     "csharp",
		"mcr.microsoft.com/dotnet/runtime:8.0": "csharp",
	}
	for ref, want := range tests {
		got, ok := classify.BaseImageLanguage(ref)
		if !ok || got != want {
			t.Errorf("BaseImageLanguage(%q) = %q, %v; want %q", ref, got, ok, want)
		}
	}
}

// An operating system, a web server, or an image nobody has heard of names no
// language. Guessing would put a language beside a component's name, which is
// the most visible thing the graph prints.
func TestBaseImagesThatNameNoLanguage(t *testing.T) {
	for _, ref := range []string{
		"scratch", "alpine:3.20", "debian:bookworm-slim", "ubuntu:24.04",
		"busybox", "gcr.io/distroless/static:nonroot",
		"nginx:1.27", "caddy:2", "postgres:16", "redis:7",
		"ghcr.io/acme/something-nobody-knows:v1", "",
	} {
		if lang, ok := classify.BaseImageLanguage(ref); ok {
			t.Errorf("BaseImageLanguage(%q) = %q, want nothing", ref, lang)
		}
	}
}

// "sdk" on its own means nothing at all, which is why the repository decides.
func TestDotnetIsRecognizedByItsRepositoryNotItsName(t *testing.T) {
	if _, ok := classify.BaseImageLanguage("acme/sdk:1"); ok {
		t.Error("a bare \"sdk\" image was read as .NET")
	}
	if _, ok := classify.BaseImageLanguage("mcr.microsoft.com/dotnet/sdk:8.0"); !ok {
		t.Error("the .NET SDK was not recognized")
	}
}

// The distinction the Dockerfile extractor needs: an unrecognized image might
// be anything, while these are known to carry no application of their own.
func TestBaseImageIsOS(t *testing.T) {
	yes := []string{
		"scratch", "SCRATCH", "alpine", "alpine:3.20", "debian:bookworm",
		"ubuntu:24.04", "busybox:latest", "gcr.io/distroless/static-debian12",
		"registry.k8s.io/debian-base-amd64:0.3",
	}
	for _, ref := range yes {
		if !classify.BaseImageIsOS(ref) {
			t.Errorf("BaseImageIsOS(%q) = false, want true", ref)
		}
	}
	no := []string{"node:20", "nginx", "postgres:16", "ghcr.io/acme/api:v1", ""}
	for _, ref := range no {
		if classify.BaseImageIsOS(ref) {
			t.Errorf("BaseImageIsOS(%q) = true, want false", ref)
		}
	}
}
