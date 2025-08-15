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
	inputFile1_muLaw := "/home/antonio/go/src/syndeo-go-gen-ai/cpp/http-server/resources/input.mulaw.raw"
	inputFile2_muLaw := "/home/antonio/go/src/syndeo-go-gen-ai/cpp/http-server/resources/last.input.twilio.original.8kHz.bin"
	outputFile := "/tmp/mixed.golib-translated-24to8.8kHz.bin"
	outputFile_16 := "/tmp/mixed.golib-translated-16to8.8kHz.bin"
	outputFile1_converted := "/tmp/output_file_24kHz.converted.16kHz.raw"
	outputFile1_muLawMixed := "/tmp/output_mu_law_mixed.8kHz.raw"

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

	file1, err = os.ReadFile(inputFile1)
	if err != nil {
		log.Fatal(err)
	}
	if resampled16kHz, err := libsamplerate.Resample24kHzTo16kHz(file1); err != nil {
		log.Fatal(err)
	} else {
		if err = os.WriteFile(outputFile1_converted, resampled16kHz, 0644); err != nil {
			log.Fatal(err)
		}
		log.Println("finished converting 24kHz to 16kHz", inputFile1, "to", outputFile1_converted)
	}

	file1, err = os.ReadFile(inputFile1_muLaw)
	if err != nil {
		log.Fatal(err)
	}
	file2, err = os.ReadFile(inputFile2_muLaw)
	if err != nil {
		log.Fatal(err)
	}

	lastPos = 1000
	if mixed8kHz, err := libsamplerate.MixUlaw8kHzDefaultFactor(file1, file2, &lastPos); err != nil {
		log.Fatal(err)
	} else {
		if err = os.WriteFile(outputFile1_muLawMixed, mixed8kHz, 0644); err != nil {
			log.Fatal(err)
		}
		log.Println("finished mixing 2 muLaw []byte", inputFile1_muLaw, "&", inputFile2_muLaw, "to", outputFile1_muLawMixed)
	}
}
