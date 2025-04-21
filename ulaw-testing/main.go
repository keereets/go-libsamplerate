package main

import (
	libsamplerate "github.com/keereets/go-libsamplerate"
	"log"
	"os"
)

func main() {
	inputFile := "/home/antonio/go/src/syndeo-go-gen-ai/cpp/http-server/resources/last.input.twilio.original.8kHz.bin"
	outputFile := "/tmp/last.input.twilio.golib-translated.16kHz.bin"

	file, err := os.ReadFile(inputFile)
	if err != nil {
		log.Fatal(err)
	}

	pcm, err := libsamplerate.ConvertUlawToPCM(file, libsamplerate.SincBestQuality)
	if err != nil {
		log.Fatal(err)
	}

	err = os.WriteFile(outputFile, pcm, 0644)
	if err != nil {
		log.Fatal(err)
	}
	log.Println("finished converting to pcm", outputFile, inputFile, len(file), "->", len(pcm))
}
