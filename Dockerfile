# Running Stage 
FROM alpine:3.20

WORKDIR /monibuca 

# Copy the pre-compiled binary from the build context
# The GitHub Actions workflow prepares 'monibuca_linux' in the context root
COPY monibuca_linux ./monibuca_linux
COPY ffmpeg /usr/local/bin/ffmpeg
COPY admin.zip ./admin.zip

# Copy the configuration file from the build context
COPY example/default/config.yaml /etc/monibuca/config.yaml

# Export necessary ports 
EXPOSE 6000 8080 8443 1935 554 5060 9000-20000
EXPOSE 5060/udp 44944/udp

CMD [ "./monibuca_linux", "-c", "/etc/monibuca/config.yaml" ]
