# Introduction
Cloudflare IP Speed Tester is a small tool written in Golang that tests the latency and download speed of some Cloudflare IP addresses and outputs the results to a CSV file.

# Install
First install Golang and Git, then run the following commands in the terminal:

```
git clone https://github.com/badafans/Cloudflare-IP-SpeedTest.git
cd Cloudflare-IP-SpeedTest
go build -o ipspeedtest main.go
```
This will compile the executable ipspeedtest.

# Parameter Description
ipspeedtest can accept the following parameters:

- file: IP address file name (default "ip.txt")
- max: maximum number of coroutines for concurrent requests (default 100)
- outfile: output file name (default "ip.csv")
- port: port (default 443)
- speedtest: The number of downloaded speed test coroutines, set to 0 to disable speed test (default 5)
- tls: whether to enable TLS (default true)
- url: Speed test file address (default "speed.cloudflare.com/__down?bytes=500000000")

# run
Run the following command in the terminal to start the program:

```
./ipspeedtest -file=ip.txt -outfile=ip.csv -port=443 -max=100 -speedtest=1 -tls=true -url=speed.cloudflare.com/__down?bytes=500000000
```
Please replace parameter values to match your actual needs.

# Output description
The program will output information for each successfully tested IP address, including IP address, port, data center, region, city, network latency and download speed (if speed test is selected).

The program also writes all results to a CSV file.

# license
The MIT License (MIT)

Here, "Software" refers to Cloudflare IP Speed Tester.

A non-restrictive license is hereby granted, permitting anyone to obtain a copy of the Software and to freely use, copy, modify, merge, publish, distribute, sublicense and/or sell copies of the Software and to bundle the Software with other software. use together.

The above copyright notice and this license notice shall be included in all copies or substantial portions of the Software.

THE SOFTWARE IS PROVIDED "AS IS", WITHOUT WARRANTY OF ANY KIND, EITHER EXPRESS OR IMPLIED, INCLUDING BUT NOT LIMITED TO THE WARRANTIES OF MERCHANTABILITY, FITNESS FOR A PARTICULAR PURPOSE, AND NON-INFRINGEMENT. IN NO EVENT SHALL THE AUTHORS OR COPYRIGHT HOLDERS BE LIABLE FOR ANY CLAIM, DAMAGES OR OTHER LIABILITY, WHETHER IN AN ACTION OF CONTRACT, TORT OR OTHERWISE, ARISING FROM, OUT OF OR IN CONNECTION WITH THE SOFTWARE OR THE USE OR OTHER DEALINGS IN THE SOFTWARE .
