FROM golang:1.5

RUN mkdir -p /go/src/app
WORKDIR /go/src/app

# this will ideally be built by the ONBUILD below ;)
CMD ["go-wrapper", "run"]
ENV GO15VENDOREXPERIMENT=1

COPY . /go/src/app
RUN go-wrapper download
RUN go-wrapper install

EXPOSE 9500
