package main

import (
    "bytes"
    "flag"
    "fmt"
    "github.com/gorilla/websocket"
    "net/http"
    "time"
)

func serveHome(w http.ResponseWriter, r *http.Request) {
    //只允许访问根路径
    if r.URL.Path != "/" {
        http.Error(w, "Not Found", http.StatusNotFound)
        return
    }
    //只允许GET请求
    if r.Method != "GET" {
        http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
        return
    }
    http.ServeFile(w, r, "home.html")
}

func main() {
    //如果命令行不指定port参数，则默认为3434
    port := flag.String("port", "3434", "http service port")
    //解析命令行输入的port参数
    flag.Parse()
    hub := NewHub()
    go hub.Run()
    //注册每种请求对应的处理函数
    http.HandleFunc("/", serveHome)
    http.HandleFunc("/ws", func(rw http.ResponseWriter, r *http.Request) {
        ServeWs(hub, rw, r)
    })
    //如果启动成功，该行会一直阻塞，hub.run()会一直运行
    if err := http.ListenAndServe(":"+*port, nil); err != nil {
        fmt.Printf("start http service error: %s\n", err)
    }
}


//hub

type Hub struct {
    clients    map[*Client]bool //维护所有的client
    broadcast  chan []byte      //广播消息
    register   chan *Client     //注册
    unregister chan *Client     //注销

}

func NewHub() *Hub {
    return &Hub{
        clients:    make(map[*Client]bool),
        broadcast:  make(chan []byte), //同步管道，确保hub消息不堆积，同时多个client给hub发数据会阻塞
        register:   make(chan *Client),
        unregister: make(chan *Client),
    }
}

func (hub *Hub) Run() {
    for {
        select {
        case client := <-hub.register:
            //client上线，注册
            hub.clients[client] = true
        case client := <-hub.unregister:
            //查询当前client是否存在
            if _, exists := hub.clients[client]; exists {
                //注销client 通道
                close(client.send)
                //删除注销的client
                delete(hub.clients, client)
            }
        case msg := <-hub.broadcast:
            //将message广播给每一位client
            for client := range hub.clients {
                select {
                case client.send <- msg:
                //异常client处理
                default:
                    close(client.send)
                    //删除异常的client
                    delete(hub.clients, client)
                }
            }
        }
    }
}


//client
var (
    pongWait         = 60 * time.Second  //等待时间
    pingPeriod       = 9 * pongWait / 10 //周期54s
    maxMsgSize int64 = 512               //消息最大长度
    writeWait        = 10 * time.Second  //
)
var (
    newLine = []byte{'\n'}
    space   = []byte{' '}
)
var upgrader = websocket.Upgrader{
    HandshakeTimeout: 2 * time.Second, //握手超时时间
    ReadBufferSize:   1024,            //读缓冲大小
    WriteBufferSize:  1024,            //写缓冲大小
    CheckOrigin:      func(r *http.Request) bool { return true },
    Error:            func(w http.ResponseWriter, r *http.Request, status int, reason error) {},
}

type Client struct {
    send      chan []byte
    hub       *Hub
    conn      *websocket.Conn
    frontName []byte //前端的名字，用于展示在消息前面
}

func (client *Client) read() {
    defer func() {
        //hub中注销client
        client.hub.unregister <- client
        fmt.Printf("close connection to %s\n", client.conn.RemoteAddr().String())
        //关闭websocket管道
        client.conn.Close()
    }()
    //一次从管管中读取的最大长度
    client.conn.SetReadLimit(maxMsgSize)
    //连接中，每隔54秒向客户端发一次ping，客户端返回pong，所以把SetReadDeadline设为60秒，超过60秒后不允许读
    _ = client.conn.SetReadDeadline(time.Now().Add(pongWait))
    //心跳
    client.conn.SetPongHandler(func(appData string) error {
        //每次收到pong都把deadline往后推迟60秒
        _ = client.conn.SetReadDeadline(time.Now().Add(pongWait))
        return nil
    })

    for {
        //如果前端主动断开连接，运行会报错，for循环会退出。注册client时，hub中会关闭client.send管道
        _, msg, err := client.conn.ReadMessage()
        if err != nil {
            //如果以意料之外的关闭状态关闭，就打印日志
            if websocket.IsUnexpectedCloseError(err, websocket.CloseAbnormalClosure, websocket.CloseGoingAway) {
                fmt.Printf("read from websocket err: %v\n", err)
            }
            //ReadMessage失败，关闭websocket管道、注销client，退出
            break
        } else {
            //换行符替换成空格，去除首尾空格
            message := bytes.TrimSpace(bytes.Replace(msg, newLine, space, -1))
            if len(client.frontName) == 0 {
                //赋给frontName，不进行广播
                client.frontName = message
                fmt.Printf("%s online\n", string(client.frontName))
            } else {
                //要广播的内容前面加上front的名字,从websocket连接里读出数据，发给hub的broadcast
                client.hub.broadcast <- bytes.Join([][]byte{client.frontName, message}, []byte(": "))
            }
        }
    }
}

//从hub的broadcast那儿读限数据，写到websocket连接里面去
func (client *Client) write() {
    //给前端发心跳，看前端是否还存活
    ticker := time.NewTicker(pingPeriod)
    defer func() {
        //ticker不用就stop，防止协程泄漏
        ticker.Stop()
        fmt.Printf("close connection to %s\n", client.conn.RemoteAddr().String())
        //给前端写数据失败，关闭连接
        client.conn.Close()
    }()

    for {
        select {
        //正常情况是hub发来了数据。如果前端断开了连接，read()会触发client.send管道的关闭，该case会立即执行。从而执行!ok里的return，从而执行defer
        case msg, ok := <-client.send:
            //client.send该管道被hub关闭
            if !ok {
                //写一条关闭信息就可以结束一切
                _ = client.conn.WriteMessage(websocket.CloseMessage, []byte{})
                return
            }
            //10秒内必须把信息写给前端（写到websocket连接里去），否则就关闭连接
            _ = client.conn.SetWriteDeadline(time.Now().Add(writeWait))
            //通过NextWriter创建一个新的writer，主要是为了确保上一个writer已经被关闭，即它想写的内容已经flush到conn里去
            if writer, err := client.conn.NextWriter(websocket.TextMessage); err != nil {
                return
            } else {
                _, _ = writer.Write(msg)
                _, _ = writer.Write(newLine) //每发一条消息，都加一个换行符
                //为了提升性能，如果client.send里还有消息，则趁这一次都写给前端
                n := len(client.send)
                for i := 0; i < n; i++ {
                    _, _ = writer.Write(<-client.send)
                    _, _ = writer.Write(newLine)
                }
                if err := writer.Close(); err != nil {
                    return //结束一切
                }
            }
        case <-ticker.C:
            _ = client.conn.SetWriteDeadline(time.Now().Add(writeWait))
            //心跳保持，给浏览器发一个PingMessage，等待浏览器返回PongMessage
            if err := client.conn.WriteMessage(websocket.PingMessage, nil); err != nil {
                return //写websocket连接失败，说明连接出问题了，该client可以over了
            }
        }
    }
}

func ServeWs(hub *Hub, w http.ResponseWriter, r *http.Request) {
    conn, err := upgrader.Upgrade(w, r, nil) //http升级为websocket协议
    if err != nil {
        fmt.Printf("upgrade error: %v\n", err)
        return
    }
    fmt.Printf("connect to client %s\n", conn.RemoteAddr().String())
    //每来一个前端请求，就会创建一个client
    client := &Client{hub: hub, conn: conn, send: make(chan []byte, 256)}
    //向hub注册client
    client.hub.register <- client

    //启动子协程，运行ServeWs的协程退出后子协程也不会能出
    //websocket是全双工模式，可以同时read和write
    go client.read()
    go client.write()
}
