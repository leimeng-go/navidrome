// Package pl implements some Data Pipeline helper functions.
// Reference: https://medium.com/amboss/applying-modern-go-concurrency-patterns-to-data-pipelines-b3b5327908d4#3a80
//
// See also:
//
//	https://www.oreilly.com/library/view/concurrency-in-go/9781491941294/ch04.html#fano_fani
//	https://www.youtube.com/watch?v=f6kdp27TYZs
//	https://www.youtube.com/watch?v=QDDwwePbDtw
package pl

import (
	"context"
	"errors"
	"sync"

	"github.com/navidrome/navidrome/log"
	"golang.org/x/sync/semaphore"
)

// Stage 构造一个流水线阶段：从输入通道读取，用至多 maxWorkers 个协程并发处理，
// 结果与错误分别送往两个输出通道。
//
// 收尾时用 context.Background() 而非传入的 ctx 等待工人结束：
// ctx 已取消的情况下仍需等所有工人退出，否则会在它们写入前就关闭输出通道从而 panic。
func Stage[In any, Out any](
	ctx context.Context,
	maxWorkers int,
	inputChannel <-chan In,
	fn func(context.Context, In) (Out, error),
) (chan Out, chan error) {
	outputChannel := make(chan Out)
	errorChannel := make(chan error)

	limit := int64(maxWorkers)
	sem1 := semaphore.NewWeighted(limit)

	go func() {
		defer close(outputChannel)
		defer close(errorChannel)

		for s := range ReadOrDone(ctx, inputChannel) {
			if err := sem1.Acquire(ctx, 1); err != nil {
				if !errors.Is(err, context.Canceled) {
					log.Error(ctx, "Failed to acquire semaphore", err)
				}
				break
			}

			go func(s In) {
				defer sem1.Release(1)

				result, err := fn(ctx, s)
				if err != nil {
					if !errors.Is(err, context.Canceled) {
						errorChannel <- err
					}
				} else {
					outputChannel <- result
				}
			}(s)
		}

		// By using context.Background() here we are assuming the fn will stop when the context
		// is canceled. This is required so we can wait for the workers to finish and avoid closing
		// the outputChannel before they are done.
		if err := sem1.Acquire(context.Background(), limit); err != nil {
			log.Error(ctx, "Failed waiting for workers", err)
		}
	}()

	return outputChannel, errorChannel
}

// Sink 是流水线终点，只关心错误，结果直接丢弃。
func Sink[In any](
	ctx context.Context,
	maxWorkers int,
	inputChannel <-chan In,
	fn func(context.Context, In) error,
) chan error {
	results, errC := Stage(ctx, maxWorkers, inputChannel, func(ctx context.Context, in In) (bool, error) {
		err := fn(ctx, in)
		return false, err // Only err is important, results will be discarded
	})

	// Discard results
	go func() {
		for range ReadOrDone(ctx, results) {
		}
	}()

	return errC
}

// Merge 扇入：把多个通道合并为一个。
func Merge[T any](ctx context.Context, cs ...<-chan T) <-chan T {
	var wg sync.WaitGroup
	out := make(chan T)

	output := func(c <-chan T) {
		defer wg.Done()
		for v := range ReadOrDone(ctx, c) {
			select {
			case out <- v:
			case <-ctx.Done():
				return
			}
		}
	}

	wg.Add(len(cs))
	for _, c := range cs {
		go output(c)
	}

	go func() {
		wg.Wait()
		close(out)
	}()

	return out
}

// SendOrDone 发送数据，context 取消时放弃，避免在无人接收时永久阻塞。
func SendOrDone[T any](ctx context.Context, out chan<- T, v T) {
	select {
	case out <- v:
	case <-ctx.Done():
		return
	}
}

// ReadOrDone 包装通道，使其在 context 取消时能及时结束，避免协程泄漏。
func ReadOrDone[T any](ctx context.Context, in <-chan T) <-chan T {
	valStream := make(chan T)
	go func() {
		defer close(valStream)
		for {
			select {
			case <-ctx.Done():
				return
			case v, ok := <-in:
				if !ok {
					return
				}
				select {
				case valStream <- v:
				case <-ctx.Done():
				}
			}
		}
	}()
	return valStream
}

// Tee 把一个通道的数据复制到两个通道。
// 每轮把已发送的那侧置为 nil，使 select 只等待剩下的一侧，从而两侧都收到同一个值。
func Tee[T any](ctx context.Context, in <-chan T) (<-chan T, <-chan T) {
	out1 := make(chan T)
	out2 := make(chan T)
	go func() {
		defer close(out1)
		defer close(out2)
		for val := range ReadOrDone(ctx, in) {
			var out1, out2 = out1, out2
			for i := 0; i < 2; i++ {
				select {
				case <-ctx.Done():
				case out1 <- val:
					out1 = nil
				case out2 <- val:
					out2 = nil
				}
			}
		}
	}()
	return out1, out2
}

// FromSlice 把切片转成带缓冲的通道，容量取切片长度故不会阻塞。
func FromSlice[T any](ctx context.Context, in []T) <-chan T {
	output := make(chan T, len(in))
	for _, c := range in {
		output <- c
	}
	close(output)
	return output
}
