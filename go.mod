module github.com/neuronlabs/neuron-extensions/server/http/api/jsonapi

replace (
	github.com/neuronlabs/neuron => ./../../../../../neuron
)

go 1.13

require (
	github.com/julienschmidt/httprouter v1.3.0
	github.com/neuronlabs/neuron v0.16.1
	github.com/neuronlabs/neuron-extensions/codec/jsonapi v0.0.0-20200809201148-e794bdb0ac7f
	github.com/neuronlabs/neuron-extensions/server/http v0.0.0-20200809201148-e794bdb0ac7f
)
