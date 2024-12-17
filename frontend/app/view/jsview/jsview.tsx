import { BlockNodeModel } from "@/app/block/blocktypes";
import * as React from "react";
import { Atom, PrimitiveAtom, atom, useAtomValue } from "jotai";
import "./jsview.scss";
import { WOS, globalStore } from "@/store/global";

import * as Babel from "@babel/standalone";
import { transform as transform } from "sucrase";
import * as services from "@/store/services";
import { loadable } from "jotai/utils";
import { base64ToString, isBlank } from "@/util/util";


function transformImports(code) {
    const regex =
      /import\s+((\*\s+as\s+\w+)|(\{\s[^}]+\s\})|(\w+))\s+from\s+"([^"]+)";/g;
    code = code.replace('from "react"', 'from "https://esm.sh/react"');
    return code.replace(regex, (match, p1, p2, p3, p4, p5) => {
      if (!p5.startsWith("https")) {
        return `import ${p1} from "https://esm.sh/${p5.replace(/@/g, "")}";`;
      }
      return match;
    });
  }
  function transformImports2(code) {
    const regex =
      /import\s+((\*\s+as\s+\w+)|(\{\s[^}]+\s\})|(\w+))\s+from\s+'([^']+)';/g;
    code = code.replace('from "react"', 'from "https://esm.sh/react"');
    return code.replace(regex, (match, p1, p2, p3, p4, p5) => {
      if (!p5.startsWith("https")) {
        return `import ${p1} from "https://esm.sh/${p5.replace(/@/g, "")}";`;
      }
      return match;
    });
  }

  function optimizeImports(jsxCode) {
    const importRegex =
      /import\s+(?:(\w+),\s+)?{([^}]*)}\s+from\s+["']([^"']+)["']/g;
    const importMap = new Map();

    // Extract import statements and group them by source
    let match;
    while ((match = importRegex.exec(jsxCode)) !== null) {
      const [fullImport, defaultImport, namedImports, source] = match;
      if (!importMap.has(source)) {
        importMap.set(source, { defaultImport: null, namedImports: new Set() });
      }
      const importData = importMap.get(source);
      if (defaultImport) {
        importData.defaultImport = defaultImport;
      }
      namedImports.split(",").forEach((namedImport) => {
        importData.namedImports.add(namedImport.trim());
      });
    }

    // Generate optimized import statements
    const optimizedImports = Array.from(importMap.entries())
      .map(([source, { defaultImport, namedImports }]) => {
        const importParts = [];
        if (defaultImport) {
          importParts.push(defaultImport);
        }
        if (namedImports.size > 0) {
          importParts.push(`{ ${Array.from(namedImports).join(", ")} }`);
        }
        return `import ${importParts.join(", ")} from "${source}";`;
      })
      .join("\n");

    // Replace the original import statements with the optimized ones
    return jsxCode.replace(importRegex, "").trim() + "\n" + optimizedImports;
  }

function transpileJSX(jsxCode) {
    jsxCode = transformImports(jsxCode);
    jsxCode = transformImports2(jsxCode);
    jsxCode = jsxCode.replace(/\`\`\`/g, "");

    const lines = jsxCode.split("\n");
    const renderLine = lines.find((line) => line.includes("ReactDOM.render"));
    if (renderLine) {
      lines.splice(lines.indexOf(renderLine), 1);
      lines.push(renderLine);
    }
    jsxCode = lines.join("\n");
    jsxCode = optimizeImports(jsxCode);

    try {
      const result = transform(jsxCode, {
        transforms: ["typescript", "jsx"],
        jsxPragma: "React.createElement",
        jsxFragmentPragma: "React.Fragment",
      });

      //console.log(result);
      jsxCode = result.code;
      //when there is a exact duplicate import in jsxCode
      const imports = jsxCode.match(/import\s+.*\s+from\s+.*;/g);
      if (imports) {
        const uniqueImports = [...new Set(imports)];
        jsxCode = jsxCode.replace(/import\s+.*\s+from\s+.*;/g, "");
        jsxCode = uniqueImports.join("\n") + jsxCode;
      }

      //when the import source from is the same, aggregate the imports
      //for example import React ,{useState } from "react"; import { useEffect, useRed} from "react";
      // should result in : import React {useState, useEffect, useRef} from "react";

      //Make sure the Line : ReactDOM.render...
      // is the last line
      // jsxCode is a string

      const newCode = Babel.transform(jsxCode, {
        presets: ["react"],
      }).code;

      
      return newCode;
    } catch (error) {
      //console.error('Erreur de syntaxe dans le code JavaScript :', error);
      // Vous pouvez également afficher un message d'erreur à l'utilisateur ici
      
      return jsxCode;
    }
  }


export class JsViewModel implements ViewModel {
    viewType: string;
    viewIcon?: Atom<string | IconButtonDecl>;
    viewName?: Atom<string>;
    view: React.FC<any>;
    blockAtom: Atom<Block>;
    connection: Atom<Promise<string>>;
    metaFilePath: Atom<string>;
    statFile: Atom<Promise<FileInfo>>;
    fileContent: Atom<Promise<string>>;
    loadableFileContent: Atom<Loadable<string>>;

    constructor(blockId: string, nodeModel: BlockNodeModel) {
        this.viewType = "js";
        this.blockAtom = WOS.getWaveObjectAtom(`block:${blockId}`);
        
        // Get file path from meta
        this.metaFilePath = atom<string>((get) => {
            const file = get(this.blockAtom)?.meta?.file;
            if (isBlank(file)) {
                return null;
            }
            return file;
        });

        // Get connection from meta
        this.connection = atom<Promise<string>>(async (get) => {
            return get(this.blockAtom)?.meta?.connection;
        });

        // Get file info
        this.statFile = atom<Promise<FileInfo>>(async (get) => {
            const fileName = get(this.metaFilePath);
            if (fileName == null) {
                return null;
            }
            const conn = (await get(this.connection)) ?? "";
            const statFile = await services.FileService.StatFile(conn, fileName);
            return statFile;
        });

        // Get file content
        this.fileContent = atom<Promise<string>>(async (get) => {
            const fileName = get(this.metaFilePath);
            if (fileName == null) {
                return null;
            }
            const conn = (await get(this.connection)) ?? "";
            const file = await services.FileService.ReadFile(conn, fileName);
            return base64ToString(file?.data64);
        });

        this.loadableFileContent = loadable(this.fileContent);
		console.log(this.loadableFileContent);
        this.view = JsView;
    }
}

function JsView({ blockId, model }: { blockId: string, model: JsViewModel }) {
	console.log(model);
    const ffile=useAtomValue(model.fileContent);
	console.log(ffile);
	
	const fileContent = useAtomValue(model.loadableFileContent);
	console.log(fileContent);
    
    // Handle loading state
    if (fileContent.state === 'loading') {
        return <div>Loading...</div>;
    }

    // Handle error state  
    if (fileContent.state === 'hasError') {
        return <div>Error loading file content</div>;
    }

    const jsxContent = fileContent.data;
    
    let fullHtmlCode = jsxContent ? 
        `<html>${htmlCode}<script ${moduleTag}>${transpileJSX(jsxContent)}
        </script>
        </html>` 
        : "";

    return (
        <div className="js-view">
            <iframe 
                srcDoc={fullHtmlCode}
                style={{ width: '100%', height: '100%', border: 'none' }}
                sandbox="allow-forms allow-modals allow-pointer-lock allow-popups allow-popups-to-escape-sandbox allow-presentation allow-same-origin allow-scripts allow-top-navigation allow-top-navigation-by-user-activation"
                allow="accelerometer; ambient-light-sensor; autoplay; battery; camera; display-capture; document-domain; encrypted-media; execution-while-not-rendered; execution-while-out-of-viewport; fullscreen; picture-in-picture; geolocation; gyroscope; layout-animations; legacy-image-formats; magnetometer; microphone; midi; navigation-override; oversized-images; payment; picture-in-picture; publickey-credentials-get; sync-xhr; usb; vr; wake-lock; xr-spatial-tracking"
                title={`js-view-${blockId}`}
            />
        </div>
    );
}

const thecontent=`
<html><div id="weather-app" class="min-h-screen bg-gray-900 text-white p-4"></div>
<script type=module>

import ReactDOM from "https://esm.sh/react-dom@18";
import wmoCodeToEmoji from 'https://esm.sh/wmo-emoji';
import React, { useState, useEffect } from "https://esm.sh/react@18";
import { setup as twindSetup } from "https://cdn.skypack.dev/twind/shim";
const _jsxFileName = "";

//appTitle: Weather App for Alès, France

;
;
twindSetup();
const WeatherApp = () => {
  const [weather, setWeather] = useState(null);
  const [loading, setLoading] = useState(true);
  useEffect(() => {
    const fetchWeather = async () => {
      setLoading(true);
      try {
        const response = await fetch("https://api.open-meteo.com/v1/forecast?latitude=44.1281&longitude=4.0839&current_weather=true");
        const data = await response.json();
        setWeather(data.current_weather);
      } catch (error) {
        console.error("Failed to fetch weather data:", error);
      } finally {
        setLoading(false);
      }
    };
    fetchWeather();
  }, []);
  const degToCompass = num => {
    var val = Math.floor(num / 22.5 + 0.5);
    var arr = ["N", "NNE", "NE", "ENE", "E", "ESE", "SE", "SSE", "S", "SSW", "SW", "WSW", "W", "WNW", "NW", "NNW"];
    return arr[val % 16];
  };
  return React.createElement('div', {
    className: "min-h-screen flex flex-col items-center justify-center",
    __self: this,
    __source: {
      fileName: _jsxFileName,
      lineNumber: 40
    }
  }, React.createElement('h1', {
    className: "text-xl font-bold mb-4",
    __self: this,
    __source: {
      fileName: _jsxFileName,
      lineNumber: 41
    }
  }, "Weather in Alès, France"), loading ? React.createElement('p', {
    __self: this,
    __source: {
      fileName: _jsxFileName,
      lineNumber: 42
    }
  }, "Loading...") : weather && React.createElement('div', {
    className: "text-center",
    __self: this,
    __source: {
      fileName: _jsxFileName,
      lineNumber: 43
    }
  }, React.createElement('p', {
    __self: this,
    __source: {
      fileName: _jsxFileName,
      lineNumber: 44
    }
  }, wmoCodeToEmoji(weather.weathercode)), React.createElement('p', {
    __self: this,
    __source: {
      fileName: _jsxFileName,
      lineNumber: 45
    }
  }, "Temperature: ", weather.temperature, "°C"), React.createElement('p', {
    __self: this,
    __source: {
      fileName: _jsxFileName,
      lineNumber: 46
    }
  }, "Wind Speed: ", weather.windspeed, " km/h"), React.createElement('p', {
    __self: this,
    __source: {
      fileName: _jsxFileName,
      lineNumber: 47
    }
  }, "Wind Direction: ", degToCompass(weather.winddirection))));
};
ReactDOM.render(React.createElement(WeatherApp, {
  __self: this,
  __source: {
    fileName: _jsxFileName,
    lineNumber: 55
  }
}), document.getElementById("weather-app"));



  </script></html>

`;

const thecontentReact=`
//appTitle: Weather App for Alès, France

import React, { useState, useEffect } from "https://esm.sh/react@18";
import ReactDOM from "https://esm.sh/react-dom@18";
import { setup as twindSetup } from 'https://cdn.skypack.dev/twind/shim';
import wmoCodeToEmoji from 'https://esm.sh/wmo-emoji';

twindSetup();

const WeatherApp = () => {
    const [weather, setWeather] = useState(null);
    const [loading, setLoading] = useState(true);

    useEffect(() => {
        const fetchWeather = async () => {
            setLoading(true);
            try {
                const response = await fetch("https://api.open-meteo.com/v1/forecast?latitude=44.1281&longitude=4.0839&current_weather=true");
                const data = await response.json();
                setWeather(data.current_weather);
            } catch (error) {
                console.error("Failed to fetch weather data:", error);
            } finally {
                setLoading(false);
            }
        };

        fetchWeather();
    }, []);

    const degToCompass = (num) => {
        var val = Math.floor((num / 22.5) + 0.5);
        var arr = ["N", "NNE", "NE", "ENE", "E", "ESE", "SE", "SSE", "S", "SSW", "SW", "WSW", "W", "WNW", "NW", "NNW"];
        return arr[(val % 16)];
    };

    return (
        <div className="min-h-screen flex flex-col items-center justify-center">
            <h1 className="text-xl font-bold mb-4">Weather in Paris, France</h1>
            {loading ? <p>Loading...</p> : weather && (
                <div className="text-center">
                    <p>{wmoCodeToEmoji(weather.weathercode)}</p>
                    <p>Temperature: {weather.temperature}°C</p>
                    <p>Wind Speed: {weather.windspeed} km/h</p>
                    <p>Wind Direction: {degToCompass(weather.winddirection)}</p>
                </div>
            )}
        </div>
    );
};

ReactDOM.render(<WeatherApp />, document.getElementById("weather-app"));

`;

const moduleTag = "type=module";
const htmlCode = `<div id="weather-app" class="min-h-screen bg-gray-900 text-white p-4"></div>`;

function makeJsViewModel(blockId: string, nodeModel: BlockNodeModel): JsViewModel {
    return new JsViewModel(blockId, nodeModel);
}

export { JsView, makeJsViewModel };