// The parts of noVNC's RFB client (https://github.com/novnc/noVNC/blob/master/docs/API.md) the desktop page uses.
export interface RFB extends EventTarget {
  scaleViewport: boolean;
  resizeSession: boolean;
  clipViewport: boolean;
  qualityLevel: number;
  compressionLevel: number;
  focusOnClick: boolean;
  viewOnly: boolean;
  background: string;
  disconnect(): void;
  focus(): void;
  clipboardPasteFrom(text: string): void;
  sendCtrlAltDel(): void;
}
// url can be a WebSocket already made, whose close code the page then sees.
export type RFBClass = new (target: HTMLElement, url: string | WebSocket, options?: object) => RFB;
