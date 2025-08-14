package main

import (
	libsamplerate "github.com/keereets/go-libsamplerate"
	"log"
	"os"
)

func main() {
	inputFile1 := "/home/antonio/go/src/syndeo-go-gen-ai/cpp/http-server/resources/mixing_input_file_24kHz.raw"
	inputFile1_16 := "/home/antonio/go/src/syndeo-go-gen-ai/cpp/http-server/resources/output.PCM16.raw"
	inputFile2 := "/home/antonio/go/src/syndeo-go-gen-ai/cpp/http-server/resources/typing.pcm24kHz.raw"
	inputFile2_16 := "/home/antonio/go/src/syndeo-go-gen-ai/cpp/http-server/resources/last.input.twilio.PCM16.output.bin"
	outputFile := "/tmp/mixed.golib-translated-24to8.8kHz.bin"
	outputFile_16 := "/tmp/mixed.golib-translated-16to8.8kHz.bin"

	file1, err := os.ReadFile(inputFile1)
	if err != nil {
		log.Fatal(err)
	}
	file2, err := os.ReadFile(inputFile2)
	if err != nil {
		log.Fatal(err)
	}

	lastPos := 0
	ulaw, err := libsamplerate.MixResampleUlaw24to8DefaultFactor(file1, file2, &lastPos)
	if err != nil {
		log.Fatal(err)
	}

	err = os.WriteFile(outputFile, ulaw, 0644)
	if err != nil {
		log.Fatal(err)
	}
	log.Println("finished mixing and converting 1st 24kHz file to uLaw", outputFile, inputFile1, inputFile2, "->", len(ulaw))

	file1, err = os.ReadFile(inputFile1_16)
	if err != nil {
		log.Fatal(err)
	}
	file2, err = os.ReadFile(inputFile2_16)
	if err != nil {
		log.Fatal(err)
	}

	lastPos = 0
	ulaw, err = libsamplerate.MixResampleUlaw16to8DefaultFactor(file1, file2, &lastPos)
	if err != nil {
		log.Fatal(err)
	}

	err = os.WriteFile(outputFile_16, ulaw, 0644)
	if err != nil {
		log.Fatal(err)
	}
	log.Println("finished mixing and converting 2nd 16kHz file to uLaw", outputFile_16, inputFile1_16, inputFile2_16, "->", len(ulaw))
}
