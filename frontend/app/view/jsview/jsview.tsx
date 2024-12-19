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

      jsxCode = result.code;
      const imports = jsxCode.match(/import\s+.*\s+from\s+.*;/g);
      if (imports) {
        const uniqueImports = [...new Set(imports)];
        jsxCode = jsxCode.replace(/import\s+.*\s+from\s+.*;/g, "");
        jsxCode = uniqueImports.join("\n") + jsxCode;
      }


      const newCode = Babel.transform(jsxCode, {
        presets: ["react"],
      }).code;

      
      return newCode;
    } catch (error) {

      return jsxCode;
    }
  }

const moduleTag = "type=module";
const htmlCode = `<div id="weather-app" class="min-h-screen bg-gray-900 text-white p-4"></div>`;



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
        `<html>
            <head>
                <meta http-equiv="Content-Security-Policy" content="default-src * 'unsafe-inline' 'unsafe-eval'; script-src * 'unsafe-inline' 'unsafe-eval'; connect-src * 'unsafe-inline'; img-src * data: blob: 'unsafe-inline'; frame-src *; style-src * 'unsafe-inline';">
            </head>
            ${htmlCode}
            
            <script ${moduleTag}>
                
                ${transpileJSX(jsxContent)}
            </script>
        </html>` 
        : "";

    return (
        <div className="js-view">
            <iframe 
                srcDoc={fullHtmlCode}
                style={{ width: '100%', height: '100%', border: 'none' }}
                sandbox="allow-scripts allow-same-origin allow-forms allow-modals allow-popups allow-downloads allow-popups-to-escape-sandbox allow-top-navigation-by-user-activation"
                allow="accelerometer; ambient-light-sensor; autoplay; battery; camera; display-capture; document-domain; encrypted-media; execution-while-not-rendered; execution-while-out-of-viewport; fullscreen; geolocation; gyroscope; layout-animations; legacy-image-formats; magnetometer; microphone; midi; payment; picture-in-picture; publickey-credentials-get; sync-xhr; usb; vr; wake-lock; xr-spatial-tracking"
                referrerPolicy="origin"
                title={`js-view-${blockId}`}
            />
        </div>
    );
}





function makeJsViewModel(blockId: string, nodeModel: BlockNodeModel): JsViewModel {
    return new JsViewModel(blockId, nodeModel);
}

export { JsView, makeJsViewModel };