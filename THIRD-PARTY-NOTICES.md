# Third-Party Notices

This file enumerates the direct dependencies compiled into `anicli`
release binaries and archives, plus the TLS-stack indirect dependencies
that materially shape the binary. Refresh the version inventory with
`make notices` and reconcile this table after dependency changes.

License texts live in each module's `LICENSE`/`COPYING` file in the Go
module cache. The full text of the one license with an advertising
clause (BSD-4-Clause, `tls-client`) is reproduced verbatim below, as
that clause requires notice reproduction in binary distributions.

## Direct dependencies

| Module | Version | License |
|---|---|---|
| `charm.land/bubbles/v2` | v2.2.1 | MIT |
| `charm.land/bubbletea/v2` | v2.0.9 | MIT |
| `charm.land/lipgloss/v2` | v2.0.6 | MIT |
| `github.com/BurntSushi/toml` | v1.6.0 | MIT |
| `github.com/PuerkitoBio/goquery` | v1.13.0 | BSD-3-Clause |
| `github.com/bogdanfinn/fhttp` | v0.6.9 | BSD-3-Clause (fork of Go `net/http`) |
| `github.com/bogdanfinn/tls-client` | v1.16.0 | **BSD-4-Clause** (text below) |
| `github.com/chromedp/chromedp` | v0.16.0 | MIT |
| `github.com/go-chi/chi/v5` | v5.3.2 | MIT |
| `github.com/jmespath/go-jmespath` | v0.4.0 | Apache-2.0 |
| `github.com/spf13/cobra` | v1.10.2 | Apache-2.0 |
| `golang.org/x/sync` | v0.23.0 | BSD-3-Clause (Go Authors) |
| `modernc.org/sqlite` | v1.58.0 | BSD-3-Clause |

## Key indirect dependencies (TLS/QUIC stack)

| Module | Version | License |
|---|---|---|
| `github.com/bogdanfinn/utls` | v1.7.8-barnius | BSD-3-Clause (Go `crypto/tls` fork) |
| `github.com/bogdanfinn/quic-go-utls` | v1.0.10-utls | MIT |

Apache-2.0 dependencies: the Apache License, Version 2.0 text is
available at <https://www.apache.org/licenses/LICENSE-2.0>. NOTICE
files, where the modules ship them, are carried in their module
trees.

## tls-client — BSD-4-Clause (reproduced verbatim)

```
Copyright (c) 2023, Bogdan Finn
All rights reserved.

Redistribution and use in source and binary forms, with or without
modification, are permitted provided that the following conditions are met:
1. Redistributions of source code must retain the above copyright
   notice, this list of conditions and the following disclaimer.
2. Redistributions in binary form must reproduce the above copyright
   notice, this list of conditions and the following disclaimer in the
   documentation and/or other materials provided with the distribution.
3. All advertising materials mentioning features or use of this software
   must display the following acknowledgement:
   This product includes software developed by the <organization>.
4. Neither the name of the <organization> nor the
   names of its contributors may be used to endorse or promote products
   derived from this software without specific prior written permission.

THIS SOFTWARE IS PROVIDED BY THE COPYRIGHT HOLDER ''AS IS'' AND ANY
EXPRESS OR IMPLIED WARRANTIES, INCLUDING, BUT NOT LIMITED TO, THE IMPLIED
WARRANTIES OF MERCHANTABILITY AND FITNESS FOR A PARTICULAR PURPOSE ARE
DISCLAIMED. IN NO EVENT SHALL THE COPYRIGHT HOLDER OR CONTRIBUTORS BE LIABLE
FOR ANY DIRECT, INDIRECT, INCIDENTAL, SPECIAL, EXEMPLARY, OR CONSEQUENTIAL
DAMAGES (INCLUDING, BUT NOT LIMITED TO, PROCUREMENT OF SUBSTITUTE GOODS OR
SERVICES; LOSS OF USE, DATA, OR PROFITS; OR BUSINESS INTERRUPTION) HOWEVER
CAUSED AND ON ANY THEORY OF LIABILITY, WHETHER IN CONTRACT, STRICT LIABILITY,
OR TORT (INCLUDING NEGLIGENCE OR OTHERWISE) ARISING IN ANY WAY OUT OF THE
USE OF THIS SOFTWARE, EVEN IF ADVISED OF THE POSSIBILITY OF SUCH DAMAGE.
```

`fhttp` and `utls` are forks of Go standard-library packages and carry
the Go Authors' BSD-3-Clause license:

```
Copyright (c) 2009 The Go Authors. All rights reserved.

Redistribution and use in source and binary forms, with or without
modification, are permitted provided that the following conditions are
met:

   * Redistributions of source code must retain the above copyright
notice, this list of conditions and the following disclaimer.
   * Redistributions in binary form must reproduce the above
copyright notice, this list of conditions and the following disclaimer
in the documentation and/or other materials provided with the
distribution.
   * Neither the name of Google Inc. nor the names of its
contributors may be used to endorse or promote products derived from
this software without specific prior written permission.

THIS SOFTWARE IS PROVIDED BY THE COPYRIGHT HOLDERS AND CONTRIBUTORS "AS IS"
AND ANY EXPRESS OR IMPLIED WARRANTIES, INCLUDING, BUT NOT LIMITED TO, THE
IMPLIED WARRANTIES OF MERCHANTABILITY AND FITNESS FOR A PARTICULAR PURPOSE
ARE DISCLAIMED. IN NO EVENT SHALL THE COPYRIGHT OWNER OR CONTRIBUTORS BE
LIABLE FOR ANY DIRECT, INDIRECT, INCIDENTAL, SPECIAL, EXEMPLARY, OR
CONSEQUENTIAL DAMAGES (INCLUDING, BUT NOT LIMITED TO, PROCUREMENT OF
SUBSTITUTE GOODS OR SERVICES; LOSS OF USE, DATA, OR PROFITS; OR BUSINESS
INTERRUPTION) HOWEVER CAUSED AND ON ANY THEORY OF LIABILITY, WHETHER IN
CONTRACT, STRICT LIABILITY, OR TORT (INCLUDING NEGLIGENCE OR OTHERWISE)
ARISING IN ANY WAY OUT OF THE USE OF THIS SOFTWARE, EVEN IF ADVISED OF THE
POSSIBILITY OF SUCH DAMAGE.
```
