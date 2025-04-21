package main

import (
	libsamplerate "github.com/keereets/go-libsamplerate"
	"log"
	"os"
)

func main() {
	inputFile1 := "/home/antonio/go/src/syndeo-go-gen-ai/cpp/http-server/resources/mixing_input_file_24kHz.raw"
	inputFile2 := "/home/antonio/go/src/syndeo-go-gen-ai/cpp/http-server/resources/typing.pcm24kHz.raw"
	outputFile := "/tmp/mixed.golib-translated.8kHz.bin"

	file1, err := os.ReadFile(inputFile1)
	if err != nil {
		log.Fatal(err)
	}
	file2, err := os.ReadFile(inputFile2)
	if err != nil {
		log.Fatal(err)
	}

	lastPos := 0
	ulaw, err := libsamplerate.MixResampleUlawDefaultFactor(file1, file2, &lastPos)
	if err != nil {
		log.Fatal(err)
	}

	err = os.WriteFile(outputFile, ulaw, 0644)
	if err != nil {
		log.Fatal(err)
	}
	log.Println("finished mixing and converting to uLaw", outputFile, inputFile1, inputFile2, "->", len(ulaw))
}
