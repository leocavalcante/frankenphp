// Package caddy provides a PHP module for the Caddy web server.
// FrankenPHP embeds the PHP interpreter directly in Caddy, giving it the ability to run your PHP scripts directly.
// No PHP FPM required!
package caddy

import (
	"encoding/json"
	"errors"
	"net/http"
	"path/filepath"
	"strconv"

	"github.com/caddyserver/caddy/v2"
	"github.com/caddyserver/caddy/v2/caddyconfig"
	"github.com/caddyserver/caddy/v2/caddyconfig/caddyfile"
	"github.com/caddyserver/caddy/v2/caddyconfig/httpcaddyfile"
	"github.com/caddyserver/caddy/v2/modules/caddyhttp"
	"github.com/caddyserver/caddy/v2/modules/caddyhttp/fileserver"
	"github.com/caddyserver/caddy/v2/modules/caddyhttp/rewrite"
	"github.com/dunglas/frankenphp"
	"go.uber.org/zap"
)

const defaultDocumentRoot = "public"

func init() {
	caddy.RegisterModule(FrankenPHPApp{})
	caddy.RegisterModule(FrankenPHPModule{})
	httpcaddyfile.RegisterGlobalOption("frankenphp", parseGlobalOption)
	httpcaddyfile.RegisterHandlerDirective("php", parseCaddyfile)
	httpcaddyfile.RegisterDirective("php_server", parsePhpServer)
}

type mainPHPinterpreterKeyType int

var mainPHPInterpreterKey mainPHPinterpreterKeyType

var phpInterpreter = caddy.NewUsagePool()

type phpInterpreterDestructor struct{}

func (phpInterpreterDestructor) Destruct() error {
	frankenphp.Shutdown()

	return nil
}

type workerConfig struct {
	// FileName sets the path to the worker script.
	FileName string `json:"file_name,omitempty"`
	// Num sets the number of workers to start.
	Num int `json:"num,omitempty"`
	// Env sets an extra environment variable to the given value. Can be specified more than once for multiple environment variables.
	Env map[string]string `json:"env,omitempty"`
}

type FrankenPHPApp struct {
	// NumThreads sets the number of PHP threads to start. Default: 2x the number of available CPUs.
	NumThreads int `json:"num_threads,omitempty"`
	// Workers configures the worker scripts to start.
	Workers []workerConfig `json:"workers,omitempty"`
}

// CaddyModule returns the Caddy module information.
func (a FrankenPHPApp) CaddyModule() caddy.ModuleInfo {
	return caddy.ModuleInfo{
		ID:  "frankenphp",
		New: func() caddy.Module { return &a },
	}
}

func (f *FrankenPHPApp) Start() error {
	repl := caddy.NewReplacer()
	logger := caddy.Log()

	opts := []frankenphp.Option{frankenphp.WithNumThreads(f.NumThreads), frankenphp.WithLogger(logger)}
	for _, w := range f.Workers {
		opts = append(opts, frankenphp.WithWorkers(repl.ReplaceKnown(w.FileName, ""), w.Num, w.Env))
	}

	_, loaded, err := phpInterpreter.LoadOrNew(mainPHPInterpreterKey, func() (caddy.Destructor, error) {
		if err := frankenphp.Init(opts...); err != nil {
			return nil, err
		}

		return phpInterpreterDestructor{}, nil
	})
	if err != nil {
		return err
	}

	if loaded {
		frankenphp.Shutdown()
		if err := frankenphp.Init(opts...); err != nil {
			return err
		}
	}

	return nil
}

func (*FrankenPHPApp) Stop() error {
	caddy.Log().Info("FrankenPHP stopped 🐘")

	return nil
}

// UnmarshalCaddyfile implements caddyfile.Unmarshaler.
func (f *FrankenPHPApp) UnmarshalCaddyfile(d *caddyfile.Dispenser) error {
	for d.Next() {
		for d.NextBlock(0) {
			switch d.Val() {
			case "num_threads":
				if !d.NextArg() {
					return d.ArgErr()
				}

				v, err := strconv.Atoi(d.Val())
				if err != nil {
					return err
				}

				f.NumThreads = v

			case "worker":
				wc := workerConfig{}
				if d.NextArg() {
					wc.FileName = d.Val()
				}

				if d.NextArg() {
					v, err := strconv.Atoi(d.Val())
					if err != nil {
						return err
					}

					wc.Num = v
				}

				for d.NextBlock(1) {
					v := d.Val()
					switch v {
					case "file":
						if !d.NextArg() {
							return d.ArgErr()
						}
						wc.FileName = d.Val()
					case "num":
						if !d.NextArg() {
							return d.ArgErr()
						}

						v, err := strconv.Atoi(d.Val())
						if err != nil {
							return err
						}

						wc.Num = v
					case "env":
						args := d.RemainingArgs()
						if len(args) != 2 {
							return d.ArgErr()
						}
						if wc.Env == nil {
							wc.Env = make(map[string]string)
						}
						wc.Env[args[0]] = args[1]
					}

					if wc.FileName == "" {
						return errors.New(`The "file" argument must be specified`)
					}

					if frankenphp.EmbeddedAppPath != "" && filepath.IsLocal(wc.FileName) {
						wc.FileName = filepath.Join(frankenphp.EmbeddedAppPath, wc.FileName)
					}
				}

				f.Workers = append(f.Workers, wc)
			}
		}
	}

	return nil
}

func parseGlobalOption(d *caddyfile.Dispenser, _ interface{}) (interface{}, error) {
	app := &FrankenPHPApp{}
	if err := app.UnmarshalCaddyfile(d); err != nil {
		return nil, err
	}

	// tell Caddyfile adapter that this is the JSON for an app
	return httpcaddyfile.App{
		Name:  "frankenphp",
		Value: caddyconfig.JSON(app, nil),
	}, nil
}

type FrankenPHPModule struct {
	// Root sets the root folder to the site. Default: `root` directive, or the path of the public directory of the embed app it exists.
	Root string `json:"root,omitempty"`
	// SplitPath sets the substrings for splitting the URI into two parts. The first matching substring will be used to split the "path info" from the path. The first piece is suffixed with the matching substring and will be assumed as the actual resource (CGI script) name. The second piece will be set to PATH_INFO for the CGI script to use. Default: `.php`.
	SplitPath []string `json:"split_path,omitempty"`
	// ResolveRootSymlink enables resolving the `root` directory to its actual value by evaluating a symbolic link, if one exists.
	ResolveRootSymlink bool `json:"resolve_root_symlink,omitempty"`
	// Env sets an extra environment variable to the given value. Can be specified more than once for multiple environment variables.
	Env    map[string]string `json:"env,omitempty"`
	logger *zap.Logger
}

// CaddyModule returns the Caddy module information.
func (FrankenPHPModule) CaddyModule() caddy.ModuleInfo {
	return caddy.ModuleInfo{
		ID:  "http.handlers.php",
		New: func() caddy.Module { return new(FrankenPHPModule) },
	}
}

// Provision sets up the module.
func (f *FrankenPHPModule) Provision(ctx caddy.Context) error {
	f.logger = ctx.Logger(f)

	if f.Root == "" {
		if frankenphp.EmbeddedAppPath == "" {
			f.Root = "{http.vars.root}"
		} else {
			f.Root = filepath.Join(frankenphp.EmbeddedAppPath, defaultDocumentRoot)
			f.ResolveRootSymlink = false
		}
	} else {
		if frankenphp.EmbeddedAppPath != "" && filepath.IsLocal(f.Root) {
			f.Root = filepath.Join(frankenphp.EmbeddedAppPath, f.Root)
		}
	}

	if len(f.SplitPath) == 0 {
		f.SplitPath = []string{".php"}
	}

	return nil
}

// ServeHTTP implements caddyhttp.MiddlewareHandler.
// TODO: Expose TLS versions as env vars, as Apache's mod_ssl: https://github.com/caddyserver/caddy/blob/master/modules/caddyhttp/reverseproxy/fastcgi/fastcgi.go#L298
func (f FrankenPHPModule) ServeHTTP(w http.ResponseWriter, r *http.Request, _ caddyhttp.Handler) error {
	origReq := r.Context().Value(caddyhttp.OriginalRequestCtxKey).(http.Request)
	repl := r.Context().Value(caddy.ReplacerCtxKey).(*caddy.Replacer)

	documentRoot := repl.ReplaceKnown(f.Root, "")

	env := make(map[string]string, len(f.Env)+1)
	env["REQUEST_URI"] = origReq.URL.RequestURI()
	for k, v := range f.Env {
		env[k] = repl.ReplaceKnown(v, "")
	}

	fr, err := frankenphp.NewRequestWithContext(
		r,
		frankenphp.WithRequestDocumentRoot(documentRoot, f.ResolveRootSymlink),
		frankenphp.WithRequestSplitPath(f.SplitPath),
		frankenphp.WithRequestEnv(env),
	)

	if err != nil {
		return err
	}

	return frankenphp.ServeHTTP(w, fr)
}

// UnmarshalCaddyfile implements caddyfile.Unmarshaler.
func (f *FrankenPHPModule) UnmarshalCaddyfile(d *caddyfile.Dispenser) error {
	for d.Next() {
		for d.NextBlock(0) {
			switch d.Val() {
			case "root":
				if !d.NextArg() {
					return d.ArgErr()
				}
				f.Root = d.Val()

			case "split":
				f.SplitPath = d.RemainingArgs()
				if len(f.SplitPath) == 0 {
					return d.ArgErr()
				}

			case "env":
				args := d.RemainingArgs()
				if len(args) != 2 {
					return d.ArgErr()
				}
				if f.Env == nil {
					f.Env = make(map[string]string)
				}
				f.Env[args[0]] = args[1]

			case "resolve_root_symlink":
				if d.NextArg() {
					return d.ArgErr()
				}
				f.ResolveRootSymlink = true
			}
		}
	}

	return nil
}

// parseCaddyfile unmarshals tokens from h into a new Middleware.
func parseCaddyfile(h httpcaddyfile.Helper) (caddyhttp.MiddlewareHandler, error) {
	m := FrankenPHPModule{}
	err := m.UnmarshalCaddyfile(h.Dispenser)

	return m, err
}

// parsePhpServer parses the php_server directive, which has a similar syntax
// to the php_fastcgi directive. A line such as this:
//
//	php_server
//
// is equivalent to a route consisting of:
//
//		# Add trailing slash for directory requests
//		@canonicalPath {
//		    file {path}/index.php
//		    not path */
//		}
//		redir @canonicalPath {path}/ 308
//
//		# If the requested file does not exist, try index files
//		@indexFiles file {
//		    try_files {path} {path}/index.php index.php
//		    split_path .php
//		}
//		rewrite @indexFiles {http.matchers.file.relative}
//
//		# FrankenPHP!
//		@phpFiles path *.php
//	 	php @phpFiles
//		file_server
//
// parsePhpServer is freely inspired from the php_fastgci directive of the Caddy server (Apache License 2.0, Matthew Holt and The Caddy Authors)
func parsePhpServer(h httpcaddyfile.Helper) ([]httpcaddyfile.ConfigValue, error) {
	if !h.Next() {
		return nil, h.ArgErr()
	}

	// set up FrankenPHP
	phpsrv := FrankenPHPModule{}

	// set up file server
	fsrv := fileserver.FileServer{}
	disableFsrv := false

	// set up the set of file extensions allowed to execute PHP code
	extensions := []string{".php"}

	// set the default index file for the try_files rewrites
	indexFile := "index.php"

	// set up for explicitly overriding try_files
	tryFiles := []string{}

	// if the user specified a matcher token, use that
	// matcher in a route that wraps both of our routes;
	// either way, strip the matcher token and pass
	// the remaining tokens to the unmarshaler so that
	// we can gain the rest of the directive syntax
	userMatcherSet, err := h.ExtractMatcherSet()
	if err != nil {
		return nil, err
	}

	// make a new dispenser from the remaining tokens so that we
	// can reset the dispenser back to this point for the
	// php unmarshaler to read from it as well
	dispenser := h.NewFromNextSegment()

	// read the subdirectives that we allow as overrides to
	// the php_server shortcut
	// NOTE: we delete the tokens as we go so that the php
	// unmarshal doesn't see these subdirectives which it cannot handle
	for dispenser.Next() {
		for dispenser.NextBlock(0) {
			// ignore any sub-subdirectives that might
			// have the same name somewhere within
			// the php passthrough tokens
			if dispenser.Nesting() != 1 {
				continue
			}

			// parse the php_server subdirectives
			switch dispenser.Val() {
			case "root":
				if !dispenser.NextArg() {
					return nil, dispenser.ArgErr()
				}
				phpsrv.Root = dispenser.Val()
				fsrv.Root = phpsrv.Root
				dispenser.DeleteN(2)

			case "split":
				extensions = dispenser.RemainingArgs()
				dispenser.DeleteN(len(extensions) + 1)
				if len(extensions) == 0 {
					return nil, dispenser.ArgErr()
				}

			case "index":
				args := dispenser.RemainingArgs()
				dispenser.DeleteN(len(args) + 1)
				if len(args) != 1 {
					return nil, dispenser.ArgErr()
				}
				indexFile = args[0]

			case "try_files":
				args := dispenser.RemainingArgs()
				dispenser.DeleteN(len(args) + 1)
				if len(args) < 1 {
					return nil, dispenser.ArgErr()
				}
				tryFiles = args

			case "file_server":
				args := dispenser.RemainingArgs()
				dispenser.DeleteN(len(args) + 1)
				if len(args) < 1 || args[0] != "off" {
					return nil, dispenser.ArgErr()
				}
				disableFsrv = true
			}
		}
	}

	// reset the dispenser after we're done so that the frankenphp
	// unmarshaler can read it from the start
	dispenser.Reset()

	if frankenphp.EmbeddedAppPath != "" {
		if phpsrv.Root == "" {
			phpsrv.Root = filepath.Join(frankenphp.EmbeddedAppPath, defaultDocumentRoot)
			fsrv.Root = phpsrv.Root
			phpsrv.ResolveRootSymlink = false
		} else if filepath.IsLocal(fsrv.Root) {
			phpsrv.Root = filepath.Join(frankenphp.EmbeddedAppPath, phpsrv.Root)
			fsrv.Root = phpsrv.Root
		}
	}

	// set up a route list that we'll append to
	routes := caddyhttp.RouteList{}

	// set the list of allowed path segments on which to split
	phpsrv.SplitPath = extensions

	// if the index is turned off, we skip the redirect and try_files
	if indexFile != "off" {
		// route to redirect to canonical path if index PHP file
		redirMatcherSet := caddy.ModuleMap{
			"file": h.JSON(fileserver.MatchFile{
				TryFiles: []string{"{http.request.uri.path}/" + indexFile},
			}),
			"not": h.JSON(caddyhttp.MatchNot{
				MatcherSetsRaw: []caddy.ModuleMap{
					{
						"path": h.JSON(caddyhttp.MatchPath{"*/"}),
					},
				},
			}),
		}
		redirHandler := caddyhttp.StaticResponse{
			StatusCode: caddyhttp.WeakString(strconv.Itoa(http.StatusPermanentRedirect)),
			Headers:    http.Header{"Location": []string{"{http.request.orig_uri.path}/"}},
		}
		redirRoute := caddyhttp.Route{
			MatcherSetsRaw: []caddy.ModuleMap{redirMatcherSet},
			HandlersRaw:    []json.RawMessage{caddyconfig.JSONModuleObject(redirHandler, "handler", "static_response", nil)},
		}

		// if tryFiles wasn't overridden, use a reasonable default
		if len(tryFiles) == 0 {
			tryFiles = []string{"{http.request.uri.path}", "{http.request.uri.path}/" + indexFile, indexFile}
		}

		// route to rewrite to PHP index file
		rewriteMatcherSet := caddy.ModuleMap{
			"file": h.JSON(fileserver.MatchFile{
				TryFiles:  tryFiles,
				SplitPath: extensions,
			}),
		}
		rewriteHandler := rewrite.Rewrite{
			URI: "{http.matchers.file.relative}",
		}
		rewriteRoute := caddyhttp.Route{
			MatcherSetsRaw: []caddy.ModuleMap{rewriteMatcherSet},
			HandlersRaw:    []json.RawMessage{caddyconfig.JSONModuleObject(rewriteHandler, "handler", "rewrite", nil)},
		}

		routes = append(routes, redirRoute, rewriteRoute)
	}

	// route to actually pass requests to PHP files;
	// match only requests that are for PHP files
	pathList := []string{}
	for _, ext := range extensions {
		pathList = append(pathList, "*"+ext)
	}
	phpMatcherSet := caddy.ModuleMap{
		"path": h.JSON(pathList),
	}

	// the rest of the config is specified by the user
	// using the php directive syntax
	dispenser.Next() // consume the directive name
	err = phpsrv.UnmarshalCaddyfile(dispenser)
	if err != nil {
		return nil, err
	}

	// create the PHP route which is
	// conditional on matching PHP files
	phpRoute := caddyhttp.Route{
		MatcherSetsRaw: []caddy.ModuleMap{phpMatcherSet},
		HandlersRaw:    []json.RawMessage{caddyconfig.JSONModuleObject(phpsrv, "handler", "php", nil)},
	}
	routes = append(routes, phpRoute)

	// create the file server route
	if !disableFsrv {
		fileRoute := caddyhttp.Route{
			MatcherSetsRaw: []caddy.ModuleMap{},
			HandlersRaw:    []json.RawMessage{caddyconfig.JSONModuleObject(fsrv, "handler", "file_server", nil)},
		}
		routes = append(routes, fileRoute)
	}

	subroute := caddyhttp.Subroute{
		Routes: routes,
	}

	// the user's matcher is a prerequisite for ours, so
	// wrap ours in a subroute and return that
	if userMatcherSet != nil {
		return []httpcaddyfile.ConfigValue{
			{
				Class: "route",
				Value: caddyhttp.Route{
					MatcherSetsRaw: []caddy.ModuleMap{userMatcherSet},
					HandlersRaw:    []json.RawMessage{caddyconfig.JSONModuleObject(subroute, "handler", "subroute", nil)},
				},
			},
		}, nil
	}

	// otherwise, return the literal subroute instead of
	// individual routes, to ensure they stay together and
	// are treated as a single unit, without necessarily
	// creating an actual subroute in the output
	return []httpcaddyfile.ConfigValue{
		{
			Class: "route",
			Value: subroute,
		},
	}, nil
}

// Interface guards
var (
	_ caddy.App                   = (*FrankenPHPApp)(nil)
	_ caddy.Provisioner           = (*FrankenPHPModule)(nil)
	_ caddyhttp.MiddlewareHandler = (*FrankenPHPModule)(nil)
	_ caddyfile.Unmarshaler       = (*FrankenPHPModule)(nil)
)
