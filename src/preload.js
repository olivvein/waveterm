let {contextBridge, ipcRenderer} = require("electron");

contextBridge.exposeInMainWorld("api", {
    getId: () => ipcRenderer.sendSync("get-id"),
    onTCmd: (callback) => ipcRenderer.on("t-cmd", callback),
    onICmd: (callback) => ipcRenderer.on("i-cmd", callback),
    onLCmd: (callback) => ipcRenderer.on("l-cmd", callback),
    onHCmd: (callback) => ipcRenderer.on("h-cmd", callback),
    onWCmd: (callback) => ipcRenderer.on("w-cmd", callback),
    onMetaArrowUp: (callback) => ipcRenderer.on("meta-arrowup", callback),
    onMetaArrowDown: (callback) => ipcRenderer.on("meta-arrowdown", callback),
    onBracketCmd: (callback) => ipcRenderer.on("bracket-cmd", callback),
    onDigitCmd: (callback) => ipcRenderer.on("digit-cmd", callback),
    contextScreen: (screenOpts, position) => ipcRenderer.send("context-screen", screenOpts, position),
});
